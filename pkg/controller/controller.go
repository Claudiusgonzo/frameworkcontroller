// MIT License
//
// Copyright (c) Microsoft Corporation. All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE

package controller

import (
	"fmt"
	ci "github.com/microsoft/frameworkcontroller/pkg/apis/frameworkcontroller/v1"
	frameworkClient "github.com/microsoft/frameworkcontroller/pkg/client/clientset/versioned"
	frameworkInformer "github.com/microsoft/frameworkcontroller/pkg/client/informers/externalversions"
	frameworkLister "github.com/microsoft/frameworkcontroller/pkg/client/listers/frameworkcontroller/v1"
	"github.com/microsoft/frameworkcontroller/pkg/common"
	"github.com/microsoft/frameworkcontroller/pkg/internal"
	errorWrap "github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	errorAgg "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeInformer "k8s.io/client-go/informers"
	kubeClient "k8s.io/client-go/kubernetes"
	coreLister "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
	"reflect"
	"sync"
	"time"
)

// FrameworkController maintains the lifecycle for all Frameworks in the cluster.
// It is the engine to transition the Framework.Status and other Framework related
// objects to satisfy the Framework.Spec eventually.
type FrameworkController struct {
	kConfig *rest.Config
	cConfig *ci.Config

	// Client is used to write remote objects in ApiServer.
	// Remote objects are up-to-date and is writable.
	//
	// To read objects, it is better to use Lister instead of Client, since the
	// Lister is cached and the cache is the ground truth of other managed objects.
	//
	// Write remote objects cannot immediately change the local cached ground truth,
	// so, it is just a hint to drive the ground truth changes, and a complete write
	// should wait until the local cached objects reflect the write.
	//
	// Client already has retry policy to retry for most transient failures.
	// Client write failure does not mean the write does not succeed on remote, the
	// failure may be due to the success response is just failed to deliver to the
	// Client.
	kClient kubeClient.Interface
	fClient frameworkClient.Interface

	// Informer is used to sync remote objects to local cached objects, and then
	// deliver corresponding events of the object changes.
	//
	// The event delivery for an object is level driven instead of edge driven,
	// and the object is identified by its name instead of its UID.
	// For example:
	// 1. Informer may not deliver any event if a create is immediately followed
	//    by a delete.
	// 2. Informer may deliver an Update event with UID changed if a delete is
	//    immediately followed by a create.
	cmInformer  cache.SharedIndexInformer
	podInformer cache.SharedIndexInformer
	fInformer   cache.SharedIndexInformer

	// Lister is used to read local cached objects in Informer.
	// Local cached objects may be outdated and is not writable.
	//
	// Outdated means current local cached objects may not reflect previous Client
	// remote writes.
	// For example, in previous round of syncFramework, Client created a Pod on
	// remote, however, in current round of syncFramework, the Pod may not appear
	// in the local cache, i.e. the local cached Pod is outdated.
	//
	// The local cached Framework.Status may be also outdated, so we take the
	// expected Framework.Status instead of the local cached one as the ground
	// truth of Framework.Status.
	//
	// The events of object changes are aligned with local cache, so we take the
	// local cached object instead of the remote one as the ground truth of
	// other managed objects except for the Framework.Status.
	// The outdated other managed object can be avoided by sync it only after the
	// remote write is also reflected in the local cache.
	cmLister  coreLister.ConfigMapLister
	podLister coreLister.PodLister
	fLister   frameworkLister.FrameworkLister

	// Queue is used to decouple items delivery and processing, i.e. control
	// how items are scheduled and distributed to process.
	// The items may come from Informer's events, or Controller's events, etc.
	//
	// It is not strictly FIFO because its Add method will only enqueue an item
	// if it is not already in the queue, i.e. the queue is deduplicated.
	// In fact, it is a FIFO pending set combined with a processing set instead of
	// a standard queue, i.e. a strict FIFO data structure.
	// So, even if we only allow to start a single worker, we cannot ensure all items
	// in the queue will be processed in FIFO order.
	// Finally, in any case, processing later enqueued item should not depend on the
	// result of processing previous enqueued item.
	//
	// However, it can be used to provide a processing lock for every different items
	// in the queue, i.e. the same item will not be processed concurrently, even in
	// the face of multiple concurrent workers.
	// Note, different items may still be processed concurrently in the face of
	// multiple concurrent workers. So, processing different items should modify
	// different objects to avoid additional concurrency control.
	//
	// Given above queue behaviors, we can choose to enqueue what kind of items:
	// 1. Framework Key
	//    Support multiple concurrent workers, but processing is coarse grained.
	//    Good at managing many small scale Frameworks.
	//    More idiomatic and easy to implement.
	// 2. All Managed Object Keys, such as Framework Key, Pod Key, etc
	//    Only support single worker, but processing is fine grained.
	//    Good at managing few large scale Frameworks.
	// 3. Events, such as [Pod p is added to Framework f]
	//    Only support single worker, and processing is fine grained.
	//    Good at managing few large scale Frameworks.
	// 4. Objects, such as Framework Object
	//    Only support single worker.
	//    Compared with local cached objects, the dequeued objects may be outdated.
	//    Internally, item will be used as map key, so objects means low performance.
	// Finally, we choose choice 1, so it is a Framework Key Queue.
	//
	// Processing is coarse grained:
	// Framework Key as item cannot differentiate Framework events, even for Add,
	// Update and Delete Framework event.
	// Besides, the dequeued item may be outdated compared the local cached one.
	// So, we can coarsen Add, Update and Delete event as a single Update event,
	// enqueue the Framework Key, and until the Framework Key is dequeued and started
	// to process, we refine the Update event to Add, Update or Delete event.
	//
	// Framework Key in queue should be valid, i.e. it can be SplitKey successfully.
	//
	// Enqueue a Framework Key means schedule a syncFramework for the Framework,
	// no matter the Framework's objects changed or not.
	//
	// Methods:
	// Add:
	//   Only keep the earliest item to dequeue:
	//   The item will only be enqueued if it is not already in the queue.
	// AddAfter:
	//   Only keep the earliest item to Add:
	//   The item may be Added before the duration elapsed, such as the same item
	//   is AddedAfter later with an earlier duration.
	fQueue workqueue.RateLimitingInterface

	// fExpectedStatusInfos is used to store the expected Framework.Status info for
	// all Frameworks.
	// See ExpectedFrameworkStatusInfo.
	//
	// Framework Key -> The expected Framework.Status info
	// Using sync.Map instead of RWMutex + map[string]*ExpectedFrameworkStatusInfo,
	// because we can ensure the same item will not be processed concurrently.
	fExpectedStatusInfos *sync.Map
}

type ExpectedFrameworkStatusInfo struct {
	// The expected Framework.Status.
	// It is the ground truth Framework.Status that the remote and the local cached
	// Framework.Status are expected to be.
	//
	// It is used to sync against the local cached Framework.Spec and the local
	// cached other related objects, and it helps to ensure the Framework.Status is
	// Monotonically Exposed.
	// Note, the local cached Framework.Status may be outdated compared with the
	// remote one, so without the it, the local cached Framework.Status is not
	// enough to ensure the Framework.Status is Monotonically Exposed.
	// See FrameworkStatus.
	status *ci.FrameworkStatus

	// The Framework.UID of the expected Framework.Status.
	uid types.UID

	// Whether the expected Framework.Status is the same as the remote one.
	// It helps to ensure the expected Framework.Status is persisted before sync.
	remoteSynced bool
}

func NewFrameworkController() *FrameworkController {
	klog.Infof("Initializing " + ci.ComponentName)

	cConfig := ci.NewConfig()
	klog.Infof("With Config: \n%v", common.ToYaml(cConfig))
	ci.AppendCompletionCodeInfos(cConfig.PodFailureSpec)

	kConfig := ci.BuildKubeConfig(cConfig)
	kClient, fClient := internal.CreateClients(kConfig)

	// Informer resync will periodically replay the event of all objects stored in its cache.
	// However, by design, Informer and Controller should not miss any event.
	// So, we should disable resync to avoid hiding missing event bugs inside Controller.
	cmListerInformer := kubeInformer.NewSharedInformerFactory(kClient, 0).Core().V1().ConfigMaps()
	podListerInformer := kubeInformer.NewSharedInformerFactory(kClient, 0).Core().V1().Pods()
	fListerInformer := frameworkInformer.NewSharedInformerFactory(fClient, 0).Frameworkcontroller().V1().Frameworks()
	cmInformer := cmListerInformer.Informer()
	podInformer := podListerInformer.Informer()
	fInformer := fListerInformer.Informer()
	cmLister := cmListerInformer.Lister()
	podLister := podListerInformer.Lister()
	fLister := fListerInformer.Lister()

	// Using DefaultControllerRateLimiter to rate limit on both particular items and overall items.
	fQueue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	c := &FrameworkController{
		kConfig:              kConfig,
		cConfig:              cConfig,
		kClient:              kClient,
		fClient:              fClient,
		cmInformer:           cmInformer,
		podInformer:          podInformer,
		fInformer:            fInformer,
		cmLister:             cmLister,
		podLister:            podLister,
		fLister:              fLister,
		fQueue:               fQueue,
		fExpectedStatusInfos: &sync.Map{},
	}

	fInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addFrameworkObj,
		UpdateFunc: c.updateFrameworkObj,
		DeleteFunc: c.deleteFrameworkObj,
	})

	cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addConfigMapObj,
		UpdateFunc: c.updateConfigMapObj,
		DeleteFunc: c.deleteConfigMapObj,
	})

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addPodObj,
		UpdateFunc: c.updatePodObj,
		DeleteFunc: c.deletePodObj,
	})

	return c
}

func (c *FrameworkController) addFrameworkObj(obj interface{}) {
	f := internal.ToFramework(obj)
	c.enqueueFrameworkObj(f, "Framework Added "+string(f.UID))
}

func (c *FrameworkController) updateFrameworkObj(oldObj, newObj interface{}) {
	oldF := internal.ToFramework(oldObj)
	newF := internal.ToFramework(newObj)
	// Informer may deliver an Update event with UID changed if a delete is
	// immediately followed by a create, so manually decompose it.
	if oldF.UID != newF.UID {
		c.deleteFrameworkObj(oldObj)
		c.addFrameworkObj(newObj)
		return
	}

	// Only care about Framework.Spec update.
	if !reflect.DeepEqual(oldF.Spec, newF.Spec) {
		c.enqueueFrameworkObj(newF, "Framework.Spec Updated")
	}
}

func (c *FrameworkController) deleteFrameworkObj(obj interface{}) {
	f := internal.ToFramework(obj)
	logSfx := ""
	if *c.cConfig.LogObjectSnapshot.Framework.OnFrameworkDeletion {
		logSfx = ci.GetFrameworkSnapshotLogTail(f)
	}
	c.enqueueFrameworkObj(f, "Framework Deleted "+string(f.UID)+logSfx)
}

func (c *FrameworkController) addConfigMapObj(obj interface{}) {
	cm := internal.ToConfigMap(obj)
	c.enqueueConfigMapObj(cm, "Framework ConfigMap Added "+string(cm.UID))
}

func (c *FrameworkController) updateConfigMapObj(oldObj, newObj interface{}) {
	oldCM := internal.ToConfigMap(oldObj)
	newCM := internal.ToConfigMap(newObj)
	// Informer may deliver an Update event with UID changed if a delete is
	// immediately followed by a create, so manually decompose it.
	if oldCM.UID != newCM.UID {
		c.deleteConfigMapObj(oldObj)
		c.addConfigMapObj(newObj)
		return
	}

	c.enqueueConfigMapObj(newCM, "Framework ConfigMap Updated")
}

func (c *FrameworkController) deleteConfigMapObj(obj interface{}) {
	cm := internal.ToConfigMap(obj)
	c.enqueueConfigMapObj(cm, "Framework ConfigMap Deleted "+string(cm.UID))
}

func (c *FrameworkController) addPodObj(obj interface{}) {
	pod := internal.ToPod(obj)
	c.enqueuePodObj(pod, "Framework Pod Added "+string(pod.UID))
}

func (c *FrameworkController) updatePodObj(oldObj, newObj interface{}) {
	oldPod := internal.ToPod(oldObj)
	newPod := internal.ToPod(newObj)
	// Informer may deliver an Update event with UID changed if a delete is
	// immediately followed by a create, so manually decompose it.
	if oldPod.UID != newPod.UID {
		c.deletePodObj(oldObj)
		c.addPodObj(newObj)
		return
	}

	c.enqueuePodObj(newPod, "Framework Pod Updated")
}

func (c *FrameworkController) deletePodObj(obj interface{}) {
	pod := internal.ToPod(obj)
	logSfx := ""
	if *c.cConfig.LogObjectSnapshot.Pod.OnPodDeletion {
		logSfx = ci.GetPodSnapshotLogTail(pod)
	}
	c.enqueuePodObj(pod, "Framework Pod Deleted "+string(pod.UID)+logSfx)
}

func (c *FrameworkController) getConfigMapOwner(cm *core.ConfigMap) *ci.Framework {
	cmOwner := meta.GetControllerOf(cm)
	if cmOwner == nil {
		return nil
	}

	if cmOwner.Kind != ci.FrameworkKind {
		return nil
	}

	f, err := c.fLister.Frameworks(cm.Namespace).Get(cmOwner.Name)
	if err != nil {
		if !apiErrors.IsNotFound(err) {
			// Unreachable
			panic(fmt.Errorf(
				"[%v]: ConfigMapOwner %#v cannot be got from local cache: %v",
				cm.Namespace+"/"+cm.Name, *cmOwner, err))
		}
		return nil
	}

	if f.UID != cmOwner.UID {
		// GarbageCollectionController will handle the dependent object
		// deletion according to the ownerReferences.
		return nil
	}

	return f
}

func (c *FrameworkController) getPodOwner(pod *core.Pod) *core.ConfigMap {
	podOwner := meta.GetControllerOf(pod)
	if podOwner == nil {
		return nil
	}

	if podOwner.Kind != ci.ConfigMapKind {
		return nil
	}

	cm, err := c.cmLister.ConfigMaps(pod.Namespace).Get(podOwner.Name)
	if err != nil {
		if !apiErrors.IsNotFound(err) {
			// Unreachable
			panic(fmt.Errorf(
				"[%v]: PodOwner %#v cannot be got from local cache: %v",
				pod.Namespace+"/"+pod.Name, *podOwner, err))
		}
		return nil
	}

	if cm.UID != podOwner.UID {
		// GarbageCollectionController will handle the dependent object
		// deletion according to the ownerReferences.
		return nil
	}

	return cm
}

func (c *FrameworkController) enqueuePodObj(pod *core.Pod, logSfx string) {
	if cm := c.getPodOwner(pod); cm != nil {
		c.enqueueConfigMapObj(cm, logSfx)
	}
}

func (c *FrameworkController) enqueueConfigMapObj(cm *core.ConfigMap, logSfx string) {
	if f := c.getConfigMapOwner(cm); f != nil {
		c.enqueueFrameworkObj(f, logSfx)
	}
}

func (c *FrameworkController) enqueueFrameworkObj(f *ci.Framework, logSfx string) {
	c.fQueue.Add(f.Key())
	klog.Infof("[%v]: enqueueFrameworkObj: %v", f.Key(), logSfx)
}

func (c *FrameworkController) Run(stopCh <-chan struct{}) {
	defer c.fQueue.ShutDown()
	defer klog.Errorf("Stopping " + ci.ComponentName)
	defer runtime.HandleCrash()

	klog.Infof("Recovering " + ci.ComponentName)
	internal.PutCRD(
		c.kConfig,
		ci.BuildFrameworkCRD(),
		c.cConfig.CRDEstablishedCheckIntervalSec,
		c.cConfig.CRDEstablishedCheckTimeoutSec)

	// The recovery order is not important, since all Frameworks will be enqueued
	// to sync in any case.
	go c.fInformer.Run(stopCh)
	go c.cmInformer.Run(stopCh)
	go c.podInformer.Run(stopCh)
	if !cache.WaitForCacheSync(
		stopCh,
		c.fInformer.HasSynced,
		c.cmInformer.HasSynced,
		c.podInformer.HasSynced) {
		panic(fmt.Errorf("Failed to WaitForCacheSync"))
	}

	klog.Infof("Running %v with %v workers",
		ci.ComponentName, *c.cConfig.WorkerNumber)

	for i := int32(0); i < *c.cConfig.WorkerNumber; i++ {
		// id is dedicated for each iteration, while i is not.
		id := i
		go wait.Until(func() { c.worker(id) }, time.Second, stopCh)
	}

	<-stopCh
}

func (c *FrameworkController) worker(id int32) {
	defer klog.Errorf("Stopping worker-%v", id)
	klog.Infof("Running worker-%v", id)

	for c.processNextWorkItem(id) {
	}
}

func (c *FrameworkController) processNextWorkItem(id int32) bool {
	// Blocked to get an item which is different from the current processing items.
	key, quit := c.fQueue.Get()
	if quit {
		return false
	}
	klog.Infof("[%v]: Assigned to worker-%v", key, id)

	// Remove the item from the current processing items to unblock getting the
	// same item again.
	defer c.fQueue.Done(key)

	err := c.syncFramework(key.(string))
	if err == nil {
		// Reset the rate limit counters of the item in the queue, such as NumRequeues,
		// because we have synced it successfully.
		c.fQueue.Forget(key)
	} else {
		c.fQueue.AddRateLimited(key)
	}

	return true
}

// It should not be invoked concurrently with the same key.
//
// Return error only for Platform Transient Error, so that the key
// can be enqueued again after rate limited delay.
// For Platform Permanent Error, it should be delivered by panic.
// For Framework Error, it should be delivered into Framework.Status.
func (c *FrameworkController) syncFramework(key string) (returnedErr error) {
	startTime := time.Now()
	logPfx := fmt.Sprintf("[%v]: syncFramework: ", key)
	klog.Infof(logPfx + "Started")
	defer func() {
		if returnedErr != nil {
			// returnedErr is already prefixed with logPfx
			klog.Warning(returnedErr.Error())
			klog.Warning(logPfx +
				"Failed to due to Platform Transient Error. " +
				"Will enqueue it again after rate limited delay")
		}
		klog.Infof(logPfx+"Completed: Duration %v", time.Since(startTime))
	}()

	fNamespace, fName := ci.SplitFrameworkKey(key)
	localF, err := c.fLister.Frameworks(fNamespace).Get(fName)
	if err != nil {
		if apiErrors.IsNotFound(err) {
			// GarbageCollectionController will handle the dependent object
			// deletion according to the ownerReferences.
			klog.Infof(logPfx+
				"Skipped: Framework cannot be found in local cache: %v", err)
			c.deleteExpectedFrameworkStatusInfo(key)
			return nil
		} else {
			return fmt.Errorf(logPfx+
				"Failed: Framework cannot be got from local cache: %v", err)
		}
	} else {
		f := localF.DeepCopy()
		// From now on, we only sync this f instance which is identified by its UID
		// instead of its name, and the f is a writable copy of the original local
		// cached one, and it may be different from the original one.
		klog.Infof(logPfx+"UID %v", f.UID)

		expected := c.getExpectedFrameworkStatusInfo(f.Key())
		if expected == nil || expected.uid != f.UID {
			if f.Status != nil {
				// Recover f related things, since it is the first time we see it and
				// its Status is not nil.
				// No need to recover previous enqueued items, because the Informer has
				// already delivered the Add events for all recovered Frameworks which
				// caused all Frameworks will be enqueued to sync.
				// No need to recover previous scheduled to enqueue items, because the
				// schedule will be recovered during sync.
			}

			// f.Status must be the same as the remote one, since it is the first
			// time we see it.
			c.updateExpectedFrameworkStatusInfo(f.Key(), f.Status, f.UID, true)
		} else {
			// f.Status may be outdated, so override it with the expected one, to
			// ensure the Framework.Status is Monotonically Exposed.
			f.Status = expected.status

			// Ensure the expected Framework.Status is the same as the remote one
			// before sync.
			if !expected.remoteSynced {
				c.compressFramework(f)
				updateErr := c.updateRemoteFrameworkStatus(f)
				c.updateExpectedFrameworkStatusInfo(f.Key(), f.Status, f.UID, updateErr == nil)

				if updateErr != nil {
					return updateErr
				}
			}
		}

		// At this point, f.Status is the same as the expected and remote
		// Framework.Status, so it is ready to sync against f.Spec and other
		// related objects.
		decompressErr := c.decompressFramework(f)
		if decompressErr != nil {
			return decompressErr
		}
		remoteRawF := f.DeepCopy()

		errs := []error{}
		syncErr := c.syncFrameworkStatus(f)
		errs = append(errs, syncErr)

		if !reflect.DeepEqual(remoteRawF.Status, f.Status) {
			// Always update the expected and remote Framework.Status even if sync
			// error, since f.Status should never be corrupted due to any Platform
			// Transient Error, so no need to rollback to the one before sync, and
			// no need to DeepCopy between f.Status and the expected one.
			c.compressFramework(f)
			updateErr := c.updateRemoteFrameworkStatus(f)
			c.updateExpectedFrameworkStatusInfo(f.Key(), f.Status, f.UID, updateErr == nil)

			errs = append(errs, updateErr)
		} else {
			klog.Infof(logPfx +
				"Skip to update the expected and remote Framework.Status since " +
				"they are unchanged")
		}

		return errorAgg.NewAggregate(errs)
	}
}

func (c *FrameworkController) enqueueFrameworkCompletedRetainTimeoutCheck(
	f *ci.Framework, failIfTimeout bool) bool {
	if f.Status.State != ci.FrameworkCompleted {
		return false
	}

	return c.enqueueFrameworkTimeoutCheck(
		f, f.Status.TransitionTime, c.cConfig.FrameworkCompletedRetainSec,
		failIfTimeout, "FrameworkCompletedRetainTimeoutCheck")
}

func (c *FrameworkController) enqueueFrameworkAttemptCreationTimeoutCheck(
	f *ci.Framework, failIfTimeout bool) bool {
	if f.Status.State != ci.FrameworkAttemptCreationRequested {
		return false
	}

	return c.enqueueFrameworkTimeoutCheck(
		f, f.Status.TransitionTime, c.cConfig.ObjectLocalCacheCreationTimeoutSec,
		failIfTimeout, "FrameworkAttemptCreationTimeoutCheck")
}

func (c *FrameworkController) enqueueTaskAttemptCreationTimeoutCheck(
	f *ci.Framework, taskRoleName string, taskIndex int32,
	failIfTimeout bool) bool {
	taskStatus := f.TaskStatus(taskRoleName, taskIndex)
	if taskStatus.State != ci.TaskAttemptCreationRequested {
		return false
	}

	return c.enqueueFrameworkTimeoutCheck(
		f, taskStatus.TransitionTime, c.cConfig.ObjectLocalCacheCreationTimeoutSec,
		failIfTimeout, "TaskAttemptCreationTimeoutCheck")
}

func (c *FrameworkController) enqueueFrameworkRetryDelayTimeoutCheck(
	f *ci.Framework, failIfTimeout bool) bool {
	if f.Status.State != ci.FrameworkAttemptCompleted {
		return false
	}

	return c.enqueueFrameworkTimeoutCheck(
		f, f.Status.TransitionTime, f.Status.RetryPolicyStatus.RetryDelaySec,
		failIfTimeout, "FrameworkRetryDelayTimeoutCheck")
}

func (c *FrameworkController) enqueueTaskRetryDelayTimeoutCheck(
	f *ci.Framework, taskRoleName string, taskIndex int32,
	failIfTimeout bool) bool {
	taskStatus := f.TaskStatus(taskRoleName, taskIndex)
	if taskStatus.State != ci.TaskAttemptCompleted {
		return false
	}

	return c.enqueueFrameworkTimeoutCheck(
		f, taskStatus.TransitionTime, taskStatus.RetryPolicyStatus.RetryDelaySec,
		failIfTimeout, "TaskRetryDelayTimeoutCheck")
}

func (c *FrameworkController) enqueuePodGracefulDeletionTimeoutCheck(
	f *ci.Framework, timeoutSec *int64,
	failIfTimeout bool, pod *core.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}

	return c.enqueueFrameworkTimeoutCheck(
		f, *internal.GetPodDeletionStartTime(pod), timeoutSec,
		failIfTimeout, "PodGracefulDeletionTimeoutCheck")
}

func (c *FrameworkController) enqueueFrameworkTimeoutCheck(
	f *ci.Framework, startTime meta.Time, timeoutSec *int64,
	failIfTimeout bool, logSfx string) bool {
	leftDuration := common.CurrentLeftDuration(startTime, timeoutSec)
	if common.IsTimeout(leftDuration) && failIfTimeout {
		return false
	}

	// The startTime may not contain OS monotonic clock, such as it is recovered
	// after FrameworkController restart. So the IsTimeout judgement may be affected
	// by OS wall clock changes, such as it should be timeout but the IsTimeout
	// returns false.
	// See wall clock and monotonic clock in Golang time/time.go.
	// To ensure the timeout will be eventually checked, AddAfter the Framework
	// for every none timeout check.
	c.fQueue.AddAfter(f.Key(), leftDuration)
	klog.Infof(
		"[%v]: enqueueFrameworkTimeoutCheck after %v: %v",
		f.Key(), leftDuration, logSfx)
	return true
}

func (c *FrameworkController) enqueueFrameworkSync(f *ci.Framework, logSfx string) {
	c.fQueue.Add(f.Key())
	klog.Infof("[%v]: enqueueFrameworkSync: %v", f.Key(), logSfx)
}

func (c *FrameworkController) syncFrameworkStatus(f *ci.Framework) error {
	logPfx := fmt.Sprintf("[%v]: syncFrameworkStatus: ", f.Key())
	klog.Infof(logPfx + "Started")
	defer func() { klog.Infof(logPfx + "Completed") }()

	if f.Status == nil {
		f.Status = f.NewFrameworkStatus()

		// To ensure FrameworkAttemptCreationPending is persisted before creating
		// its cm, we need to wait until next sync to create the cm, so manually
		// enqueue a sync.
		c.enqueueFrameworkSync(f, "FrameworkAttemptCreationPending")
		klog.Infof(logPfx + "Waiting FrameworkAttemptCreationPending to be persisted")
		return nil
	} else {
		if c.syncFrameworkScale(f) || c.compactFrameworkScale(f) {
			// To ensure TaskAttemptCreationPending is persisted before creating
			// its pod, we need to wait until next sync to create the pod, so manually
			// enqueue a sync.
			// To ensure the Task[DeletionPending] is persisted before deleting its pod
			// or deleting/replacing its Task instance, we need to wait until next sync
			// to delete its pod or delete/replace the Task instance, so manually enqueue
			// a sync.
			c.enqueueFrameworkSync(f, "TaskAttemptCreationPending/Task[DeletionPending]")
			klog.Infof(logPfx +
				"Waiting TaskAttemptCreationPending/Task[DeletionPending] to be persisted")
			return nil
		}

		if c.updatePodGracefulDeletionTimeoutSec(f) {
			// To ensure PodGracefulDeletionTimeoutSec is persisted before gracefully
			// delete any pod, we need to wait until next sync to gracefully delete,
			// so manually enqueue a sync.
			c.enqueueFrameworkSync(f, "Task[PodGracefulDeletionTimeoutSec][Changed]")
			klog.Infof(logPfx +
				"Waiting Task[PodGracefulDeletionTimeoutSec][Changed] to be persisted")
			return nil
		}
	}

	return c.syncFrameworkState(f)
}

// Rescale not Completing/Completed Framework according to its current f.Spec.
// After this, all ScaleUp TaskRoles and Tasks are added, and all ScaleDown Tasks
// are marked as DeletionPending for later lazy graceful deletion, thus:
// 1. TaskRoles/Tasks in f.Status must fully contain TaskRoles/Tasks in f.Spec.
// 2. TaskRoles/Tasks in f.Spec must fully contain not DeletionPending (ScaleDown)
//    TaskRoles/Tasks in f.Status.
//
// This helps to ensure the Rescale is effective immediately, as essentially,
// ScaleUp/ScaleDown is to setup/destroy the relationship between Framework and
// its TaskRoles/Tasks, which does not have to wait until, such as
// FrameworkAttemptInstance (ConfigMap) is created or any DeletionPending
// (ScaleDown) TaskAttemptInstance (Pod) is gracefully deleted.
func (c *FrameworkController) syncFrameworkScale(
	f *ci.Framework) (producedNewPendingTask bool) {
	logPfx := fmt.Sprintf("[%v]: syncFrameworkScale: ", f.Key())
	klog.Infof(logPfx + "Started")
	defer func() { klog.Infof(logPfx + "Completed") }()

	producedNewPendingTask = false

	// No longer react to Rescale after the whole FrameworkAttempt Completing,
	// to ensure DeletionPending (ScaleDown) Task will never trigger (impact)
	// Framework/FrameworkAttempt completion.
	if f.IsCompleting() ||
		f.Status.State == ci.FrameworkAttemptCompleted ||
		f.Status.State == ci.FrameworkCompleted {
		klog.Infof(logPfx+"Skipped: Framework is already %v", f.Status.State)
		return producedNewPendingTask
	}

	for _, taskRoleSpec := range f.Spec.TaskRoles {
		taskRoleName := taskRoleSpec.Name
		taskCountSpec := taskRoleSpec.TaskNumber
		taskRoleStatus := f.GetTaskRoleStatus(taskRoleName)

		if taskRoleStatus == nil {
			// ScaleUp: Directly add TaskRole that need to bring up.
			klog.Infof("[%v][%v]: syncFrameworkScale: ScaleUp: Goal: %v -> %v",
				f.Key(), taskRoleName, nil, taskCountSpec)

			trs := ci.TaskRoleStatus{Name: taskRoleName, TaskStatuses: []*ci.TaskStatus{}}
			for taskIndex := int32(0); taskIndex < taskCountSpec; taskIndex++ {
				trs.TaskStatuses =
					append(trs.TaskStatuses, f.NewTaskStatus(taskRoleName, taskIndex))
				producedNewPendingTask = true
			}
			f.Status.AttemptStatus.TaskRoleStatuses =
				append(f.Status.AttemptStatus.TaskRoleStatuses, &trs)
		} else {
			taskCountStatus := int32(len(taskRoleStatus.TaskStatuses))
			if taskCountStatus < taskCountSpec {
				// ScaleUp: Directly add Task that need to bring up.
				klog.Infof("[%v][%v]: syncFrameworkScale: ScaleUp: Goal: %v -> %v",
					f.Key(), taskRoleName, taskCountStatus, taskCountSpec)

				for taskIndex := taskCountStatus; taskIndex < taskCountSpec; taskIndex++ {
					taskRoleStatus.TaskStatuses =
						append(taskRoleStatus.TaskStatuses, f.NewTaskStatus(taskRoleName, taskIndex))
					producedNewPendingTask = true
				}
			} else if taskCountStatus > taskCountSpec {
				// ScaleDown: Just mark Task that need to bring down as DeletionPending.
				klog.Infof("[%v][%v]: syncFrameworkScale: ScaleDown: Goal: %v -> %v",
					f.Key(), taskRoleName, taskCountStatus, taskCountSpec)

				for taskIndex := taskCountStatus - 1; taskIndex >= taskCountSpec; taskIndex-- {
					taskStatus := taskRoleStatus.TaskStatuses[taskIndex]
					if taskStatus.MarkAsDeletionPending() {
						producedNewPendingTask = true
					}
				}
			}
		}
	}

	for _, taskRoleStatus := range f.TaskRoleStatuses() {
		taskRoleName := taskRoleStatus.Name
		taskCountStatus := int32(len(taskRoleStatus.TaskStatuses))
		taskRoleSpec := f.GetTaskRoleSpec(taskRoleName)

		if taskRoleSpec == nil {
			// ScaleDown: Just mark Task that need to bring down as DeletionPending.
			klog.Infof("[%v][%v]: syncFrameworkScale: ScaleDown: Goal: %v -> %v",
				f.Key(), taskRoleName, taskCountStatus, nil)

			for taskIndex := taskCountStatus - 1; taskIndex >= 0; taskIndex-- {
				taskStatus := taskRoleStatus.TaskStatuses[taskIndex]
				if taskStatus.MarkAsDeletionPending() {
					producedNewPendingTask = true
				}
			}
		}
	}

	return producedNewPendingTask
}

// Compact not Completing/Completed Framework scale by cleaning up its Completed
// DeletionPending TaskRoles/Tasks.
// It drives the Completed DeletionPending TaskRoles/Tasks to be deleted or
// replaced by new Task instance.
// Before calling it, ensure the Completed DeletionPending TaskRoles/Tasks has
// been persisted, so it is safe to also expose them as history snapshots here.
func (c *FrameworkController) compactFrameworkScale(
	f *ci.Framework) (producedNewPendingTask bool) {
	logPfx := fmt.Sprintf("[%v]: compactFrameworkScale: ", f.Key())
	klog.Infof(logPfx + "Started")
	defer func() { klog.Infof(logPfx + "Completed") }()

	producedNewPendingTask = false

	// Align with syncFrameworkScale to simplify completing.
	if f.IsCompleting() ||
		f.Status.State == ci.FrameworkAttemptCompleted ||
		f.Status.State == ci.FrameworkCompleted {
		klog.Infof(logPfx+"Skipped: Framework is already %v", f.Status.State)
		return producedNewPendingTask
	}

	// For TaskRoles/Tasks which no longer belong to its current f.Spec, try to
	// delete the Completed DeletionPending ones.
	taskRoleStatuses := &f.Status.AttemptStatus.TaskRoleStatuses
	for taskRoleIndex := len(*taskRoleStatuses) - 1; taskRoleIndex >= 0; taskRoleIndex-- {
		taskRoleStatus := (*taskRoleStatuses)[taskRoleIndex]
		taskRoleName := taskRoleStatus.Name
		taskCountStatus := int32(len(taskRoleStatus.TaskStatuses))
		// Will delete Tasks in in range [taskIndexDeleteStart, taskCountStatus)
		taskIndexDeleteStart := taskCountStatus

		taskRoleSpec := f.GetTaskRoleSpec(taskRoleName)
		var taskCountSpec int32
		if taskRoleSpec == nil {
			taskCountSpec = 0
		} else {
			taskCountSpec = taskRoleSpec.TaskNumber
		}

		for taskIndex := taskCountStatus - 1; taskIndex >= taskCountSpec; taskIndex-- {
			taskStatus := taskRoleStatus.TaskStatuses[taskIndex]
			if taskStatus.DeletionPending && taskStatus.State == ci.TaskCompleted {
				taskIndexDeleteStart = taskIndex
			} else {
				// Cannot continue graceful deletion anymore
				break
			}
		}

		var newTaskCountStatus *int32
		if taskIndexDeleteStart == 0 && taskRoleSpec == nil {
			// Delete the whole Completed DeletionPending TaskRole
			newTaskCountStatus = nil
		} else {
			// Delete tail Completed DeletionPending Tasks
			newTaskCountStatus = &taskIndexDeleteStart
		}

		if newTaskCountStatus != nil && *newTaskCountStatus == taskCountStatus {
			// Nothing can be deleted
			continue
		}

		// Start deletion
		logSfx := ""
		if *c.cConfig.LogObjectSnapshot.Framework.OnFrameworkRescale {
			// Ensure the FrameworkSnapshot is exposed before the deletion.
			logSfx = ci.GetFrameworkSnapshotLogTail(f)
		}
		klog.Info(fmt.Sprintf(
			"[%v][%v]: compactFrameworkScale: ScaleDown: Deletion: %v -> %v",
			f.Key(), taskRoleName, taskCountStatus,
			common.SprintPtrInt32(newTaskCountStatus)) + logSfx)

		if newTaskCountStatus == nil {
			taskRoleLastIndex := len(*taskRoleStatuses) - 1
			(*taskRoleStatuses)[taskRoleIndex] = (*taskRoleStatuses)[taskRoleLastIndex]
			(*taskRoleStatuses)[taskRoleLastIndex] = nil
			*taskRoleStatuses = (*taskRoleStatuses)[:taskRoleLastIndex]
		} else {
			for taskIndex := taskCountStatus - 1; taskIndex >= *newTaskCountStatus; taskIndex-- {
				taskRoleStatus.TaskStatuses[taskIndex] = nil
			}
			taskRoleStatus.TaskStatuses = taskRoleStatus.TaskStatuses[:*newTaskCountStatus]
		}
	}

	// For TaskRoles/Tasks which still belong to its current f.Spec, replace all
	// Completed DeletionPending ones with new Task instances.
	for _, taskRoleStatus := range f.TaskRoleStatuses() {
		taskRoleName := taskRoleStatus.Name
		taskCountStatus := int32(len(taskRoleStatus.TaskStatuses))
		taskRoleSpec := f.GetTaskRoleSpec(taskRoleName)

		if taskRoleSpec != nil {
			taskCountSpec := taskRoleSpec.TaskNumber
			taskCountStatusAndSpec := common.MinInt32(taskCountStatus, taskCountSpec)
			for taskIndex := taskCountStatusAndSpec - 1; taskIndex >= 0; taskIndex-- {
				taskStatus := taskRoleStatus.TaskStatuses[taskIndex]

				if taskStatus.DeletionPending && taskStatus.State == ci.TaskCompleted {
					// Replace the Completed DeletionPending Task with new instance
					logSfx := ""
					if *c.cConfig.LogObjectSnapshot.Framework.OnFrameworkRescale {
						// Ensure the FrameworkSnapshot is exposed before the deletion.
						logSfx = ci.GetFrameworkSnapshotLogTail(f)
					}
					klog.Info(fmt.Sprintf(
						"[%v][%v][%v]: compactFrameworkScale: ScaleDown: Replacement",
						f.Key(), taskRoleName, taskIndex) + logSfx)

					taskRoleStatus.TaskStatuses[taskIndex] =
						f.NewTaskStatus(taskRoleName, taskIndex)
					producedNewPendingTask = true
				}
			}
		}
	}

	return producedNewPendingTask
}

func (c *FrameworkController) updatePodGracefulDeletionTimeoutSec(
	f *ci.Framework) (changed bool) {
	logPfx := fmt.Sprintf("[%v]: updatePodGracefulDeletionTimeoutSec: ", f.Key())
	klog.Infof(logPfx + "Started")
	defer func() { klog.Infof(logPfx + "Completed") }()

	changed = false

	if f.Status.State == ci.FrameworkCompleted {
		klog.Infof(logPfx+"Skipped: Framework is already %v", f.Status.State)
		return changed
	}

	for _, taskRoleSpec := range f.Spec.TaskRoles {
		taskRoleName := taskRoleSpec.Name
		taskRoleStatus := f.GetTaskRoleStatus(taskRoleName)
		if taskRoleStatus == nil {
			// Unreachable
			continue
		}

		if !common.EqualsPtrInt64(
			taskRoleStatus.PodGracefulDeletionTimeoutSec,
			taskRoleSpec.Task.PodGracefulDeletionTimeoutSec) {
			taskRoleStatus.PodGracefulDeletionTimeoutSec =
				common.DeepCopyInt64(taskRoleSpec.Task.PodGracefulDeletionTimeoutSec)
			changed = true
		}
	}

	return changed
}

// Sync Framework with other related objects.
// It also drives the DeletionPending TaskRoles/Tasks to be Completed.
func (c *FrameworkController) syncFrameworkState(f *ci.Framework) (err error) {
	logPfx := fmt.Sprintf("[%v]: syncFrameworkState: ", f.Key())
	klog.Infof(logPfx + "Started")
	defer func() { klog.Infof(logPfx + "Completed") }()

	if f.Status.State == ci.FrameworkCompleted {
		if c.enqueueFrameworkCompletedRetainTimeoutCheck(f, true) {
			klog.Infof(logPfx+"Skipped: Framework is already %v, "+
				"and waiting to be deleted after FrameworkCompletedRetainSec",
				f.Status.State)
			return nil
		}

		// deleteFramework
		logSfx := ""
		if *c.cConfig.LogObjectSnapshot.Framework.OnFrameworkDeletion {
			// Ensure the FrameworkSnapshot is exposed before the deletion.
			logSfx = ci.GetFrameworkSnapshotLogTail(f)
		}
		klog.Info(logPfx + fmt.Sprintf("Framework will be deleted due to "+
			"FrameworkCompletedRetainSec %v is expired",
			common.SecToDuration(c.cConfig.FrameworkCompletedRetainSec)) + logSfx)
		return c.deleteFramework(f, true)
	}

	var cm *core.ConfigMap
	if f.Status.State != ci.FrameworkAttemptCompleted {
		// ConfigMap may have been creation requested successfully and may exist in
		// remote, so need to sync against it.
		cm, err = c.getOrCleanupConfigMap(f, false)
		if err != nil {
			return err
		}

		if cm == nil {
			// Avoid sync with outdated object:
			// cm is remote creation requested but not found in the local cache.
			if f.Status.State == ci.FrameworkAttemptCreationRequested {
				var diag string
				var code ci.CompletionCode
				if f.Spec.ExecutionType == ci.ExecutionStop {
					diag = "User has requested to stop the Framework"
					code = ci.CompletionCodeStopFrameworkRequested
					klog.Info(logPfx + diag)
				} else {
					if c.enqueueFrameworkAttemptCreationTimeoutCheck(f, true) {
						klog.Infof(logPfx +
							"Waiting ConfigMap to appear in the local cache or timeout")
						return nil
					}

					diag = fmt.Sprintf(
						"ConfigMap does not appear in the local cache within timeout %v, "+
							"so consider it was deleted and explicitly delete it",
						common.SecToDuration(c.cConfig.ObjectLocalCacheCreationTimeoutSec))
					code = ci.CompletionCodeConfigMapCreationTimeout
					klog.Warning(logPfx + diag)
				}

				// Ensure cm is deleted in remote to avoid managed cm leak after
				// FrameworkAttemptCompleted.
				err := c.deleteConfigMap(f, *f.ConfigMapUID(), true)
				if err != nil {
					return err
				}

				c.completeFrameworkAttempt(f, true,
					code.NewFrameworkAttemptCompletionStatus(diag, nil))
				return nil
			}

			if f.Status.State != ci.FrameworkAttemptCreationPending {
				if f.Status.AttemptStatus.CompletionStatus == nil {
					diag := fmt.Sprintf("ConfigMap was deleted by others")
					klog.Warning(logPfx + diag)
					c.completeFrameworkAttempt(f, true,
						ci.CompletionCodeConfigMapExternalDeleted.
							NewFrameworkAttemptCompletionStatus(diag, nil))
				} else {
					c.completeFrameworkAttempt(f, true, nil)
				}

				return nil
			}
		} else {
			if cm.DeletionTimestamp == nil {
				if f.Status.State == ci.FrameworkAttemptDeletionPending {
					// The CompletionStatus has been persisted, so it is safe to delete the
					// cm now.
					err := c.deleteConfigMap(f, *f.ConfigMapUID(), false)
					if err != nil {
						return err
					}
					f.TransitionFrameworkState(ci.FrameworkAttemptDeletionRequested)
				}

				// Avoid sync with outdated object:
				// cm is remote deletion requested but not deleting or deleted in the local
				// cache.
				if f.Status.State == ci.FrameworkAttemptDeletionRequested {
					// The deletion requested object will never appear again with the same UID,
					// so always just wait.
					klog.Infof(logPfx +
						"Waiting ConfigMap to disappearing or disappear in the local cache")
				} else {
					// At this point, f.Status.State must be in:
					// {FrameworkAttemptCreationRequested, FrameworkAttemptPreparing,
					// FrameworkAttemptRunning}

					if f.Status.State == ci.FrameworkAttemptCreationRequested {
						f.TransitionFrameworkState(ci.FrameworkAttemptPreparing)
					}
				}
			} else {
				if f.Status.AttemptStatus.CompletionStatus == nil {
					diag := fmt.Sprintf("ConfigMap is being deleted by others")
					klog.Warning(logPfx + diag)
					f.Status.AttemptStatus.CompletionStatus =
						ci.CompletionCodeConfigMapExternalDeleted.
							NewFrameworkAttemptCompletionStatus(diag, nil)
				}

				f.TransitionFrameworkState(ci.FrameworkAttemptDeleting)
				klog.Infof(logPfx + "Waiting ConfigMap to be deleted")
			}
		}
	}
	// At this point, f.Status.State must be in:
	// {FrameworkAttemptCreationPending, FrameworkAttemptPreparing,
	// FrameworkAttemptRunning, FrameworkAttemptDeletionRequested,
	// FrameworkAttemptDeleting, FrameworkAttemptCompleted}

	if f.Status.State == ci.FrameworkAttemptCompleted {
		// attemptToRetryFramework
		retryDecision := f.Spec.RetryPolicy.ShouldRetry(
			f.Status.RetryPolicyStatus,
			f.Status.AttemptStatus.CompletionStatus.CompletionStatus,
			*c.cConfig.FrameworkMinRetryDelaySecForTransientConflictFailed,
			*c.cConfig.FrameworkMaxRetryDelaySecForTransientConflictFailed)

		if f.Status.RetryPolicyStatus.RetryDelaySec == nil {
			// RetryFramework is not yet scheduled, so need to be decided.
			if retryDecision.ShouldRetry {
				// scheduleToRetryFramework
				klog.Infof(logPfx+
					"Will retry Framework with new FrameworkAttempt: RetryDecision: %v",
					retryDecision)

				f.Status.RetryPolicyStatus.RetryDelaySec = &retryDecision.DelaySec
			} else {
				// completeFramework
				klog.Infof(logPfx+
					"Will complete Framework: RetryDecision: %v",
					retryDecision)

				f.TransitionFrameworkState(ci.FrameworkCompleted)

				c.enqueueFrameworkCompletedRetainTimeoutCheck(f, false)
				klog.Infof(logPfx +
					"Waiting Framework to be deleted after FrameworkCompletedRetainSec")
				return nil
			}
		}

		if f.Status.RetryPolicyStatus.RetryDelaySec != nil {
			// RetryFramework is already scheduled, so just need to check whether it
			// should be executed now.
			if f.Spec.ExecutionType == ci.ExecutionStop {
				klog.Infof(logPfx +
					"User has requested to stop the Framework, " +
					"so immediately retry without delay")
			} else {
				if c.enqueueFrameworkRetryDelayTimeoutCheck(f, true) {
					klog.Infof(logPfx + "Waiting Framework to retry after delay")
					return nil
				}
			}

			// retryFramework
			logSfx := ""
			if *c.cConfig.LogObjectSnapshot.Framework.OnFrameworkRetry {
				// The completed FrameworkAttempt has been persisted, so it is safe to
				// also expose it as one history snapshot.
				logSfx = ci.GetFrameworkSnapshotLogTail(f)
			}
			klog.Info(logPfx + "Framework will be retried" + logSfx)

			f.Status.RetryPolicyStatus.TotalRetriedCount++
			if retryDecision.IsAccountable {
				f.Status.RetryPolicyStatus.AccountableRetriedCount++
			}
			f.Status.RetryPolicyStatus.RetryDelaySec = nil
			f.Status.AttemptStatus = f.NewFrameworkAttemptStatus(
				f.Status.RetryPolicyStatus.TotalRetriedCount)
			f.TransitionFrameworkState(ci.FrameworkAttemptCreationPending)

			// To ensure FrameworkAttemptCreationPending is persisted before creating
			// its cm, we need to wait until next sync to create the cm, so manually
			// enqueue a sync.
			c.enqueueFrameworkSync(f, "FrameworkAttemptCreationPending")
			klog.Infof(logPfx + "Waiting FrameworkAttemptCreationPending to be persisted")
			return nil
		}
	}
	// At this point, f.Status.State must be in:
	// {FrameworkAttemptCreationPending, FrameworkAttemptPreparing,
	// FrameworkAttemptRunning, FrameworkAttemptDeletionRequested,
	// FrameworkAttemptDeleting}

	if f.Status.State == ci.FrameworkAttemptCreationPending {
		if f.DeletionTimestamp != nil {
			klog.Infof(logPfx + "Skip to createFrameworkAttempt: " +
				"Framework is deleting")
			return nil
		}

		if f.Spec.ExecutionType == ci.ExecutionStop {
			diag := "User has requested to stop the Framework"
			klog.Info(logPfx + diag)

			// Ensure cm is deleted in remote to avoid managed cm leak after
			// FrameworkAttemptCompleted.
			_, err = c.getOrCleanupConfigMap(f, true)
			if err != nil {
				return err
			}

			c.completeFrameworkAttempt(f, true,
				ci.CompletionCodeStopFrameworkRequested.
					NewFrameworkAttemptCompletionStatus(diag, nil))
			return nil
		}

		// createFrameworkAttempt
		cm, err = c.createConfigMap(f)
		if err != nil {
			return err
		}

		f.Status.AttemptStatus.ConfigMapUID = &cm.UID
		f.Status.AttemptStatus.InstanceUID = ci.GetFrameworkAttemptInstanceUID(
			f.FrameworkAttemptID(), f.ConfigMapUID())
		f.TransitionFrameworkState(ci.FrameworkAttemptCreationRequested)

		// Informer may not deliver any event if a create is immediately followed by
		// a delete, so manually enqueue a sync to check the cm existence after the
		// timeout.
		c.enqueueFrameworkAttemptCreationTimeoutCheck(f, false)

		// The ground truth cm is the local cached one instead of the remote one,
		// so need to wait before continue the sync.
		klog.Infof(logPfx +
			"Waiting ConfigMap to appear in the local cache or timeout")
		return nil
	}
	// At this point, f.Status.State must be in:
	// {FrameworkAttemptPreparing, FrameworkAttemptRunning,
	// FrameworkAttemptDeletionRequested, FrameworkAttemptDeleting}

	if f.Status.State == ci.FrameworkAttemptPreparing ||
		f.Status.State == ci.FrameworkAttemptRunning ||
		f.Status.State == ci.FrameworkAttemptDeletionRequested ||
		f.Status.State == ci.FrameworkAttemptDeleting {
		if !f.IsCompleting() {
			if f.Spec.ExecutionType == ci.ExecutionStop {
				diag := "User has requested to stop the Framework"
				klog.Info(logPfx + diag)
				c.completeFrameworkAttempt(f, false,
					ci.CompletionCodeStopFrameworkRequested.
						NewFrameworkAttemptCompletionStatus(diag, nil))
			}
		}

		if !f.IsCompleting() {
			c.syncFrameworkAttemptCompletionPolicy(f)
		}

		err := c.syncTaskRoleStatuses(f, cm)

		if f.Status.State == ci.FrameworkAttemptPreparing {
			if f.IsAnyTaskRunning(true) {
				f.TransitionFrameworkState(ci.FrameworkAttemptRunning)
			}
		}

		return err
	} else {
		// Unreachable
		panic(fmt.Errorf(logPfx+
			"Failed: At this point, FrameworkState should be in "+
			"{%v, %v, %v, %v} instead of %v",
			ci.FrameworkAttemptPreparing, ci.FrameworkAttemptRunning,
			ci.FrameworkAttemptDeletionRequested, ci.FrameworkAttemptDeleting,
			f.Status.State))
	}
}

func (c *FrameworkController) deleteFramework(
	f *ci.Framework, confirm bool) error {
	errPfx := fmt.Sprintf(
		"[%v]: Failed to delete Framework %v: confirm: %v: ",
		f.Key(), f.UID, confirm)

	deleteErr := c.fClient.FrameworkcontrollerV1().Frameworks(f.Namespace).Delete(
		f.Name, &meta.DeleteOptions{
			Preconditions:     &meta.Preconditions{UID: &f.UID},
			PropagationPolicy: common.PtrDeletionPropagation(meta.DeletePropagationForeground),
		})
	if deleteErr != nil {
		if !apiErrors.IsNotFound(deleteErr) {
			return fmt.Errorf(errPfx+"%v", deleteErr)
		}
	} else {
		if confirm {
			// Confirm it is deleted instead of still deleting.
			remoteF, getErr := c.fClient.FrameworkcontrollerV1().Frameworks(f.Namespace).Get(
				f.Name, meta.GetOptions{})
			if getErr != nil {
				if !apiErrors.IsNotFound(getErr) {
					return fmt.Errorf(errPfx+
						"Framework cannot be got from remote: %v", getErr)
				}
			} else {
				if f.UID == remoteF.UID {
					return fmt.Errorf(errPfx+
						"Framework with DeletionTimestamp %v still exist after deletion",
						remoteF.DeletionTimestamp)
				}
			}
		}
	}

	klog.Infof(
		"[%v]: Succeeded to delete Framework %v: confirm: %v",
		f.Key(), f.UID, confirm)
	return nil
}

// Get Framework's current ConfigMap object, if not found, then clean up existing
// controlled ConfigMap if any.
// Returned cm is either managed or nil, if it is the managed cm, it is not
// writable and may be outdated even if no error.
// Clean up instead of recovery is because the ConfigMapUID is always the ground
// truth.
func (c *FrameworkController) getOrCleanupConfigMap(
	f *ci.Framework, confirm bool) (cm *core.ConfigMap, err error) {
	logPfx := fmt.Sprintf("[%v]: getOrCleanupConfigMap: ", f.Key())
	cmName := f.ConfigMapName()

	if confirm {
		cm, err = c.kClient.CoreV1().ConfigMaps(f.Namespace).Get(cmName,
			meta.GetOptions{})
	} else {
		cm, err = c.cmLister.ConfigMaps(f.Namespace).Get(cmName)
	}

	if err != nil {
		if apiErrors.IsNotFound(err) {
			return nil, nil
		} else {
			return nil, fmt.Errorf(logPfx+
				"Failed to get ConfigMap %v: confirm: %v: %v",
				cmName, confirm, err)
		}
	}

	if f.ConfigMapUID() == nil || *f.ConfigMapUID() != cm.UID {
		// cm is the unmanaged
		if meta.IsControlledBy(cm, f) {
			// The managed ConfigMap becomes unmanaged if and only if Framework.Status
			// is failed to persist due to FrameworkController restart or create fails
			// but succeeds on remote, so clean up the ConfigMap to avoid unmanaged cm
			// leak.
			klog.Warningf(logPfx+
				"Found unmanaged but controlled ConfigMap, so explicitly delete it: %v, %v",
				cm.Name, cm.UID)
			return nil, c.deleteConfigMap(f, cm.UID, confirm)
		} else {
			// Do not own and manage the life cycle of not controlled object, so still
			// consider the get and controlled object clean up is success, and postpone
			// the potential naming conflict when creating the controlled object.
			klog.Warningf(logPfx+
				"Found unmanaged and uncontrolled ConfigMap, and it may be naming conflict "+
				"with the controlled ConfigMap to be created: %v, %v",
				cm.Name, cm.UID)
			return nil, nil
		}
	} else {
		// cm is the managed
		return cm, nil
	}
}

// Using UID to ensure we delete the right object.
// The cmUID should be controlled by f.
func (c *FrameworkController) deleteConfigMap(
	f *ci.Framework, cmUID types.UID, confirm bool) error {
	cmName := f.ConfigMapName()
	errPfx := fmt.Sprintf(
		"[%v]: Failed to delete ConfigMap %v, %v: confirm: %v: ",
		f.Key(), cmName, cmUID, confirm)

	deleteErr := c.kClient.CoreV1().ConfigMaps(f.Namespace).Delete(cmName,
		&meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &cmUID}})
	if deleteErr != nil {
		if !apiErrors.IsNotFound(deleteErr) {
			return fmt.Errorf(errPfx+"%v", deleteErr)
		}
	} else {
		if confirm {
			// Confirm it is deleted instead of still deleting.
			cm, getErr := c.kClient.CoreV1().ConfigMaps(f.Namespace).Get(cmName,
				meta.GetOptions{})
			if getErr != nil {
				if !apiErrors.IsNotFound(getErr) {
					return fmt.Errorf(errPfx+
						"ConfigMap cannot be got from remote: %v", getErr)
				}
			} else {
				if cmUID == cm.UID {
					return fmt.Errorf(errPfx+
						"ConfigMap with DeletionTimestamp %v still exist after deletion",
						cm.DeletionTimestamp)
				}
			}
		}
	}

	klog.Infof(
		"[%v]: Succeeded to delete ConfigMap %v, %v: confirm: %v",
		f.Key(), cmName, cmUID, confirm)
	return nil
}

func (c *FrameworkController) createConfigMap(
	f *ci.Framework) (*core.ConfigMap, error) {
	cm := f.NewConfigMap()
	errPfx := fmt.Sprintf(
		"[%v]: Failed to create ConfigMap %v: ",
		f.Key(), cm.Name)

	remoteCM, createErr := c.kClient.CoreV1().ConfigMaps(f.Namespace).Create(cm)
	if createErr != nil {
		if apiErrors.IsAlreadyExists(createErr) {
			// Best effort to judge if conflict with a not controlled object.
			localCM, getErr := c.cmLister.ConfigMaps(f.Namespace).Get(cm.Name)
			if getErr == nil && !meta.IsControlledBy(localCM, f) {
				return nil, fmt.Errorf(errPfx+
					"ConfigMap naming conflicts with others: "+
					"Existing ConfigMap %v with DeletionTimestamp %v is not "+
					"controlled by current Framework %v, %v: %v",
					localCM.UID, localCM.DeletionTimestamp, f.Name, f.UID, createErr)
			}
		}

		return nil, fmt.Errorf(errPfx+"%v", createErr)
	} else {
		klog.Infof(
			"[%v]: Succeeded to create ConfigMap %v",
			f.Key(), cm.Name)
		return remoteCM, nil
	}
}

// FrameworkAttemptCompletionPolicy can be triggered by not only completed Tasks
// increased in f.Status, but also FrameworkAttemptCompletionPolicy or TotalTaskCount
// decreased in f.Spec, so full sync here is needed.
// Note, the sync is relatively very cheap, so it is fine to call the sync during
// all kinds of FrameworkSync.
func (c *FrameworkController) syncFrameworkAttemptCompletionPolicy(
	f *ci.Framework) (completionPolicyTriggered bool) {
	logPfx := fmt.Sprintf("[%v]: syncFrameworkAttemptCompletionPolicy: ", f.Key())
	klog.Infof(logPfx + "Started")
	defer func() { klog.Infof(logPfx + "Completed") }()

	failedTaskSelector := ci.BindIDP((*ci.TaskStatus).IsFailed, true)
	succeededTaskSelector := ci.BindIDP((*ci.TaskStatus).IsSucceeded, true)
	completedTaskSelector := ci.BindIDP((*ci.TaskStatus).IsCompleted, true)

	var firstTriggerTime *meta.Time
	var firstTriggerCompletionStatus *ci.FrameworkAttemptCompletionStatus
	for _, taskRoleSpec := range f.Spec.TaskRoles {
		taskRoleName := taskRoleSpec.Name
		taskRoleStatus := f.GetTaskRoleStatus(taskRoleName)
		if taskRoleStatus == nil {
			// Unreachable
			continue
		}

		completionPolicy := taskRoleSpec.FrameworkAttemptCompletionPolicy
		minFailedTaskCount := completionPolicy.MinFailedTaskCount
		minSucceededTaskCount := completionPolicy.MinSucceededTaskCount

		if minFailedTaskCount >= 1 {
			failedTaskCount := taskRoleStatus.GetTaskCountStatus(failedTaskSelector)
			if failedTaskCount >= minFailedTaskCount {
				trigger := taskRoleStatus.CompletionTimeOrderedTaskStatus(
					failedTaskSelector, minFailedTaskCount-1)

				if firstTriggerTime == nil || trigger.CompletionTime.Before(firstTriggerTime) {
					firstTriggerTime = trigger.CompletionTime
					firstTriggerCompletionStatus = ci.NewFailedTaskTriggeredCompletionStatus(
						trigger, taskRoleName, failedTaskCount, minFailedTaskCount)
				}
			}
		}

		if minSucceededTaskCount >= 1 {
			succeededTaskCount := taskRoleStatus.GetTaskCountStatus(succeededTaskSelector)
			if succeededTaskCount >= minSucceededTaskCount {
				trigger := taskRoleStatus.CompletionTimeOrderedTaskStatus(
					succeededTaskSelector, minSucceededTaskCount-1)

				if firstTriggerTime == nil || trigger.CompletionTime.Before(firstTriggerTime) {
					firstTriggerTime = trigger.CompletionTime
					firstTriggerCompletionStatus = ci.NewSucceededTaskTriggeredCompletionStatus(
						trigger, taskRoleName, succeededTaskCount, minSucceededTaskCount)
				}
			}
		}
	}

	if firstTriggerCompletionStatus != nil {
		klog.Infof("[%v][%v][%v]: syncFrameworkAttemptCompletionPolicy: %v", f.Key(),
			firstTriggerCompletionStatus.Trigger.TaskRoleName,
			firstTriggerCompletionStatus.Trigger.TaskIndex,
			firstTriggerCompletionStatus.Trigger.Message)
		c.completeFrameworkAttempt(f, false, firstTriggerCompletionStatus)
		return true
	}

	// The Framework must not Completing or Completed, so TaskRoles/Tasks in
	// f.Spec must fully contain not DeletionPending (ScaleDown) TaskRoles/Tasks
	// in f.Status, thus completedTaskCount must <= totalTaskCount.
	totalTaskCount := f.GetTotalTaskCountSpec()
	completedTaskCount := f.GetTaskCountStatus(completedTaskSelector)
	if completedTaskCount >= totalTaskCount {
		var lastCompletedTaskStatus *ci.TaskStatus
		var lastCompletedTaskRoleName string
		for _, taskRoleSpec := range f.Spec.TaskRoles {
			taskRoleName := taskRoleSpec.Name
			taskRoleStatus := f.GetTaskRoleStatus(taskRoleName)
			if taskRoleStatus == nil {
				// Unreachable
				continue
			}

			roleTotalTaskCount := taskRoleSpec.TaskNumber
			if roleTotalTaskCount == 0 {
				continue
			}

			roleLastCompletedTask := taskRoleStatus.CompletionTimeOrderedTaskStatus(
				completedTaskSelector, roleTotalTaskCount-1)

			if lastCompletedTaskStatus == nil ||
				roleLastCompletedTask.CompletionTime.Time.After(
					lastCompletedTaskStatus.CompletionTime.Time) {
				lastCompletedTaskStatus = roleLastCompletedTask
				lastCompletedTaskRoleName = taskRoleName
			}
		}

		firstTriggerCompletionStatus = ci.NewCompletedTaskTriggeredCompletionStatus(
			lastCompletedTaskStatus, lastCompletedTaskRoleName,
			completedTaskCount, totalTaskCount)

		if firstTriggerCompletionStatus.Trigger == nil {
			klog.Infof("[%v]: syncFrameworkAttemptCompletionPolicy: %v", f.Key(),
				firstTriggerCompletionStatus.Diagnostics)
		} else {
			klog.Infof("[%v][%v][%v]: syncFrameworkAttemptCompletionPolicy: %v", f.Key(),
				firstTriggerCompletionStatus.Trigger.TaskRoleName,
				firstTriggerCompletionStatus.Trigger.TaskIndex,
				firstTriggerCompletionStatus.Trigger.Message)
		}
		c.completeFrameworkAttempt(f, false, firstTriggerCompletionStatus)
		return true
	}

	return false
}

func (c *FrameworkController) syncTaskRoleStatuses(
	f *ci.Framework, cm *core.ConfigMap) (err error) {
	logPfx := fmt.Sprintf("[%v]: syncTaskRoleStatuses: ", f.Key())
	klog.Infof(logPfx + "Started")
	defer func() { klog.Infof(logPfx + "Completed") }()

	errs := []error{}
	for _, taskRoleStatus := range f.TaskRoleStatuses() {
		klog.Infof("[%v][%v]: syncTaskRoleStatus", f.Key(), taskRoleStatus.Name)
		for _, taskStatus := range taskRoleStatus.TaskStatuses {
			// At this point, f.Status.State must be in:
			// {FrameworkAttemptPreparing, FrameworkAttemptRunning,
			// FrameworkAttemptDeletionPending, FrameworkAttemptDeletionRequested,
			// FrameworkAttemptDeleting}
			err := c.syncTaskState(f, cm, taskRoleStatus.Name, taskStatus.Index)
			if err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errorAgg.NewAggregate(errs)
}

func (c *FrameworkController) syncTaskState(
	f *ci.Framework, cm *core.ConfigMap,
	taskRoleName string, taskIndex int32) (err error) {
	logPfx := fmt.Sprintf("[%v][%v][%v]: syncTaskState: ",
		f.Key(), taskRoleName, taskIndex)
	klog.Infof(logPfx + "Started")
	defer func() { klog.Infof(logPfx + "Completed") }()

	taskRoleSpec := f.GetTaskRoleSpec(taskRoleName)
	taskRoleStatus := f.TaskRoleStatus(taskRoleName)
	taskStatus := f.TaskStatus(taskRoleName, taskIndex)

	if taskStatus.State == ci.TaskCompleted {
		// The TaskCompleted has already been considered during above
		// syncFrameworkAttemptCompletionPolicy, so it is safe to skip below
		// attemptToCompleteFrameworkAttempt.
		//
		// If the Task is DeletionPending, since it is not deleted, this must be caused
		// by other Task behind it has not yet TaskCompleted.
		// And if the following Task becomes TaskCompleted later, a sync will be
		// enqueued to trigger the deletion.
		klog.Infof(logPfx + "Skipped: Task is already completed")
		return nil
	}

	var pod *core.Pod
	if taskStatus.State != ci.TaskAttemptCompleted {
		// Pod may have been creation requested successfully and may exist in remote,
		// so need to sync against it.
		pod, err = c.getOrCleanupPod(f, cm, taskRoleName, taskIndex, false)
		if err != nil {
			return err
		}

		if pod == nil {
			// Avoid sync with outdated object:
			// pod is remote creation requested but not found in the local cache.
			if taskStatus.State == ci.TaskAttemptCreationRequested {
				var diag string
				var code ci.CompletionCode
				if taskStatus.DeletionPending {
					diag = "User has requested to delete the Task by Framework ScaleDown"
					code = ci.CompletionCodeDeleteTaskRequested
					klog.Info(logPfx + diag)
				} else {
					if c.enqueueTaskAttemptCreationTimeoutCheck(f, taskRoleName, taskIndex, true) {
						klog.Infof(logPfx +
							"Waiting Pod to appear in the local cache or timeout")
						return nil
					}

					diag = fmt.Sprintf(
						"Pod does not appear in the local cache within timeout %v, "+
							"so consider it was deleted and explicitly delete it",
						common.SecToDuration(c.cConfig.ObjectLocalCacheCreationTimeoutSec))
					code = ci.CompletionCodePodCreationTimeout
					klog.Warning(logPfx + diag)
				}

				// Ensure pod is deleted in remote to avoid managed pod leak after
				// TaskAttemptCompleted.
				err := c.deletePod(f, taskRoleName, taskIndex, *taskStatus.PodUID(), true, false)
				if err != nil {
					return err
				}

				c.completeTaskAttempt(f, taskRoleName, taskIndex, true,
					code.NewTaskAttemptCompletionStatus(diag, nil))
				return nil
			}

			if taskStatus.State != ci.TaskAttemptCreationPending {
				if taskStatus.AttemptStatus.CompletionStatus == nil {
					diag := fmt.Sprintf("Pod was deleted by others")
					klog.Warning(logPfx + diag)
					c.completeTaskAttempt(f, taskRoleName, taskIndex, true,
						ci.CompletionCodePodExternalDeleted.
							NewTaskAttemptCompletionStatus(diag, nil))
				} else {
					c.completeTaskAttempt(f, taskRoleName, taskIndex, true, nil)
				}

				return nil
			}
		} else {
			if pod.DeletionTimestamp == nil {
				if taskStatus.State == ci.TaskAttemptDeletionPending {
					// The CompletionStatus has been persisted, so it is safe to delete the
					// pod now.
					err := c.deletePod(f, taskRoleName, taskIndex, *taskStatus.PodUID(), false, false)
					if err != nil {
						return err
					}
					f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskAttemptDeletionRequested)
				}

				// Avoid sync with outdated object:
				// pod is remote deletion requested but not deleting or deleted in the local
				// cache.
				if taskStatus.State == ci.TaskAttemptDeletionRequested {
					// The deletion requested object will never appear again with the same UID,
					// so always just wait.
					klog.Infof(logPfx +
						"Waiting Pod to disappearing or disappear in the local cache")
					return nil
				}

				// At this point, taskStatus.State must be in:
				// {TaskAttemptCreationRequested, TaskAttemptPreparing, TaskAttemptRunning}
				if taskStatus.State == ci.TaskAttemptCreationRequested {
					f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskAttemptPreparing)
				}

				// Below Pod fields may be available even when PodPending, such as the Pod
				// has been bound to a Node, but one or more Containers has not been started.
				taskStatus.AttemptStatus.PodNodeName = &pod.Spec.NodeName
				taskStatus.AttemptStatus.PodIP = &pod.Status.PodIP
				taskStatus.AttemptStatus.PodHostIP = &pod.Status.HostIP

				if pod.Status.Phase == core.PodUnknown {
					// Possibly due to the NodeController has not heard from the kubelet who
					// manages the Pod for more than node-monitor-grace-period but less than
					// pod-eviction-timeout.
					// And after pod-eviction-timeout, the Pod will be marked as deleting, but
					// it will only be automatically deleted after the kubelet comes back and
					// kills the Pod.
					klog.Infof(logPfx+
						"Waiting Pod to be deleted or deleting or transitioned from %v",
						pod.Status.Phase)
				} else if pod.Status.Phase == core.PodPending {
					f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskAttemptPreparing)
				} else if pod.Status.Phase == core.PodRunning {
					f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskAttemptRunning)
				} else if pod.Status.Phase == core.PodSucceeded {
					diag := fmt.Sprintf("Pod succeeded")
					klog.Info(logPfx + diag)
					c.completeTaskAttempt(f, taskRoleName, taskIndex, false,
						ci.CompletionCodeSucceeded.NewTaskAttemptCompletionStatus(
							diag, ci.ExtractPodCompletionStatus(pod)))
					return nil
				} else if pod.Status.Phase == core.PodFailed {
					result := ci.MatchCompletionCodeInfos(pod)
					diag := fmt.Sprintf("Pod failed: %v", result.Diagnostics)
					klog.Info(logPfx + diag)
					c.completeTaskAttempt(f, taskRoleName, taskIndex, false,
						&ci.TaskAttemptCompletionStatus{
							CompletionStatus: &ci.CompletionStatus{
								Code:        *result.CodeInfo.Code,
								Phrase:      result.CodeInfo.Phrase,
								Type:        result.CodeInfo.Type,
								Diagnostics: diag,
							},
							Pod: ci.ExtractPodCompletionStatus(pod),
						},
					)
					return nil
				} else {
					return fmt.Errorf(logPfx+
						"Failed: Got unrecognized Pod Phase: %v", pod.Status.Phase)
				}
			} else {
				if taskStatus.AttemptStatus.CompletionStatus == nil {
					diag := fmt.Sprintf("Pod is being deleted by others")
					klog.Warning(logPfx + diag)
					taskStatus.AttemptStatus.CompletionStatus =
						ci.CompletionCodePodExternalDeleted.
							NewTaskAttemptCompletionStatus(diag, nil)
				}

				f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskAttemptDeleting)
				return c.handlePodGracefulDeletion(f, taskRoleName, taskIndex, pod)
			}
		}
	}
	// At this point, taskStatus.State must be in:
	// {TaskAttemptCreationPending, TaskAttemptPreparing,
	// TaskAttemptRunning, TaskAttemptCompleted}

	if taskStatus.State == ci.TaskAttemptPreparing ||
		taskStatus.State == ci.TaskAttemptRunning {
		if taskStatus.DeletionPending {
			diag := "User has requested to delete the Task by Framework ScaleDown"
			klog.Info(logPfx + diag)
			c.completeTaskAttempt(f, taskRoleName, taskIndex, false,
				ci.CompletionCodeDeleteTaskRequested.
					NewTaskAttemptCompletionStatus(diag, nil))
		}
		return nil
	}
	// At this point, taskStatus.State must be in:
	// {TaskAttemptCreationPending, TaskAttemptCompleted}

	if taskStatus.State == ci.TaskAttemptCompleted {
		// attemptToRetryTask
		var retryDecision ci.RetryDecision
		if taskRoleSpec == nil {
			retryDecision = ci.RetryDecision{
				ShouldRetry: false, IsAccountable: true,
				DelaySec: 0, Reason: "TaskRoleSpec is already deleted"}
		} else {
			retryDecision = taskRoleSpec.Task.RetryPolicy.ShouldRetry(
				taskStatus.RetryPolicyStatus,
				taskStatus.AttemptStatus.CompletionStatus.CompletionStatus,
				0, 0)
		}

		if taskStatus.RetryPolicyStatus.RetryDelaySec == nil {
			// RetryTask is not yet scheduled, so need to be decided.
			if retryDecision.ShouldRetry {
				// scheduleToRetryTask
				klog.Infof(logPfx+
					"Will retry Task with new TaskAttempt: RetryDecision: %v",
					retryDecision)

				taskStatus.RetryPolicyStatus.RetryDelaySec = &retryDecision.DelaySec
			} else {
				// completeTask
				klog.Infof(logPfx+
					"Will complete Task: RetryDecision: %v",
					retryDecision)

				f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskCompleted)
			}
		}

		if taskStatus.RetryPolicyStatus.RetryDelaySec != nil {
			// RetryTask is already scheduled, so just need to check whether it
			// should be executed now.
			if taskStatus.DeletionPending {
				klog.Infof(logPfx +
					"User has requested to delete the Task by Framework ScaleDown, " +
					"so immediately retry without delay")
			} else {
				if c.enqueueTaskRetryDelayTimeoutCheck(f, taskRoleName, taskIndex, true) {
					klog.Infof(logPfx + "Waiting Task to retry after delay")
					return nil
				}
			}

			// retryTask
			logSfx := ""
			if *c.cConfig.LogObjectSnapshot.Framework.OnTaskRetry {
				// The completed TaskAttempt has been persisted, so it is safe to also
				// expose it as one history snapshot.
				logSfx = ci.GetFrameworkSnapshotLogTail(f)
			}
			klog.Info(logPfx + "Task will be retried" + logSfx)

			taskStatus.RetryPolicyStatus.TotalRetriedCount++
			if retryDecision.IsAccountable {
				taskStatus.RetryPolicyStatus.AccountableRetriedCount++
			}
			taskStatus.RetryPolicyStatus.RetryDelaySec = nil
			taskStatus.AttemptStatus = f.NewTaskAttemptStatus(
				taskRoleName, taskIndex, taskStatus.RetryPolicyStatus.TotalRetriedCount)
			f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskAttemptCreationPending)

			// To ensure TaskAttemptCreationPending is persisted before creating
			// its pod, we need to wait until next sync to create the pod, so manually
			// enqueue a sync.
			c.enqueueFrameworkSync(f, "TaskAttemptCreationPending")
			klog.Infof(logPfx + "Waiting TaskAttemptCreationPending to be persisted")
			return nil
		}
	}
	// At this point, taskStatus.State must be in:
	// {TaskAttemptCreationPending, TaskCompleted}

	if taskStatus.State == ci.TaskAttemptCreationPending {
		if f.IsCompleting() {
			klog.Infof(logPfx + "Skip to createTaskAttempt: " +
				"FrameworkAttempt is completing")
			return nil
		}

		if taskStatus.DeletionPending || taskRoleSpec == nil {
			diag := "User has requested to delete the Task by Framework ScaleDown"
			klog.Info(logPfx + diag)

			// Ensure pod is deleted in remote to avoid managed pod leak after
			// TaskAttemptCompleted.
			_, err = c.getOrCleanupPod(f, cm, taskRoleName, taskIndex, true)
			if err != nil {
				return err
			}

			c.completeTaskAttempt(f, taskRoleName, taskIndex, true,
				ci.CompletionCodeDeleteTaskRequested.
					NewTaskAttemptCompletionStatus(diag, nil))
			return nil
		}

		// createTaskAttempt
		pod, err = c.createPod(f, cm, taskRoleName, taskIndex)
		if err != nil {
			apiErr := errorWrap.Cause(err)
			if internal.IsPodSpecPermanentError(apiErr) {
				// Should be Framework Error instead of Platform Transient Error.
				diag := fmt.Sprintf("Failed to create Pod: %v", common.ToJson(apiErr))
				klog.Info(logPfx + diag)

				// Ensure pod is deleted in remote to avoid managed pod leak after
				// TaskAttemptCompleted.
				_, err = c.getOrCleanupPod(f, cm, taskRoleName, taskIndex, true)
				if err != nil {
					return err
				}

				c.completeTaskAttempt(f, taskRoleName, taskIndex, true,
					ci.CompletionCodePodSpecPermanentError.
						NewTaskAttemptCompletionStatus(diag, nil))
				return nil
			} else {
				return err
			}
		}

		taskStatus.AttemptStatus.PodUID = &pod.UID
		taskStatus.AttemptStatus.InstanceUID = ci.GetTaskAttemptInstanceUID(
			taskStatus.TaskAttemptID(), taskStatus.PodUID())
		f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskAttemptCreationRequested)

		// Informer may not deliver any event if a create is immediately followed by
		// a delete, so manually enqueue a sync to check the pod existence after the
		// timeout.
		c.enqueueTaskAttemptCreationTimeoutCheck(f, taskRoleName, taskIndex, false)

		// The ground truth pod is the local cached one instead of the remote one,
		// so need to wait before continue the sync.
		klog.Infof(logPfx +
			"Waiting Pod to appear in the local cache or timeout")
		return nil
	}
	// At this point, taskStatus.State must be in:
	// {TaskCompleted}

	if taskStatus.State == ci.TaskCompleted {
		if f.IsCompleting() {
			klog.Infof(logPfx + "Skip to attemptToCompleteFrameworkAttempt: " +
				"FrameworkAttempt is completing")
			return nil
		}

		if taskStatus.DeletionPending || taskRoleSpec == nil {
			klog.Infof(logPfx + "Skip to attemptToCompleteFrameworkAttempt: " +
				"Task is DeletionPending")

			// To ensure the TaskCompleted[DeletionPending] is persisted before
			// deleting/replacing its Task instance, we need to wait until next
			// sync to delete/replace the Task instance, so manually enqueue a sync.
			c.enqueueFrameworkSync(f, "TaskCompleted[DeletionPending]")
			klog.Infof(logPfx + "Waiting TaskCompleted[DeletionPending] to be persisted")
			return nil
		}

		// attemptToCompleteFrameworkAttempt
		failedTaskSelector := ci.BindIDP((*ci.TaskStatus).IsFailed, true)
		succeededTaskSelector := ci.BindIDP((*ci.TaskStatus).IsSucceeded, true)
		completedTaskSelector := ci.BindIDP((*ci.TaskStatus).IsCompleted, true)

		completionPolicy := taskRoleSpec.FrameworkAttemptCompletionPolicy
		minFailedTaskCount := completionPolicy.MinFailedTaskCount
		minSucceededTaskCount := completionPolicy.MinSucceededTaskCount

		var triggerCompletionStatus *ci.FrameworkAttemptCompletionStatus
		if taskStatus.IsFailed(true) && minFailedTaskCount >= 1 {
			failedTaskCount := taskRoleStatus.GetTaskCountStatus(failedTaskSelector)
			if failedTaskCount >= minFailedTaskCount {
				triggerCompletionStatus = ci.NewFailedTaskTriggeredCompletionStatus(
					taskStatus, taskRoleName, failedTaskCount, minFailedTaskCount)
			}
		}

		if taskStatus.IsSucceeded(true) && minSucceededTaskCount >= 1 {
			succeededTaskCount := taskRoleStatus.GetTaskCountStatus(succeededTaskSelector)
			if succeededTaskCount >= minSucceededTaskCount {
				triggerCompletionStatus = ci.NewSucceededTaskTriggeredCompletionStatus(
					taskStatus, taskRoleName, succeededTaskCount, minSucceededTaskCount)
			}
		}

		if triggerCompletionStatus != nil {
			klog.Info(logPfx + triggerCompletionStatus.Trigger.Message)
			c.completeFrameworkAttempt(f, false, triggerCompletionStatus)
			return nil
		}

		// The Framework must not Completing or Completed, so TaskRoles/Tasks in
		// f.Spec must fully contain not DeletionPending (ScaleDown) TaskRoles/Tasks
		// in f.Status, thus completedTaskCount must <= totalTaskCount.
		totalTaskCount := f.GetTotalTaskCountSpec()
		completedTaskCount := f.GetTaskCountStatus(completedTaskSelector)
		if completedTaskCount >= totalTaskCount {
			triggerCompletionStatus = ci.NewCompletedTaskTriggeredCompletionStatus(
				taskStatus, taskRoleName, completedTaskCount, totalTaskCount)

			klog.Info(logPfx + triggerCompletionStatus.Trigger.Message)
			c.completeFrameworkAttempt(f, false, triggerCompletionStatus)
			return nil
		}

		return nil
	}
	// At this point, taskStatus.State must be in:
	// {}

	// Unreachable
	panic(fmt.Errorf(logPfx+
		"Failed: At this point, TaskState should be in {} instead of %v",
		taskStatus.State))
}

// The pod should be controlled by f's cm.
func (c *FrameworkController) handlePodGracefulDeletion(
	f *ci.Framework, taskRoleName string, taskIndex int32, pod *core.Pod) error {
	logPfx := fmt.Sprintf("[%v][%v][%v]: handlePodGracefulDeletion: ",
		f.Key(), taskRoleName, taskIndex)
	taskStatus := f.TaskRoleStatus(taskRoleName)
	timeoutSec := taskStatus.PodGracefulDeletionTimeoutSec

	if pod.DeletionTimestamp == nil {
		return nil
	}
	if timeoutSec == nil {
		klog.Infof(logPfx + "Waiting Pod to be deleted")
		return nil
	}
	if c.enqueuePodGracefulDeletionTimeoutCheck(f, timeoutSec, true, pod) {
		klog.Infof(logPfx + "Waiting Pod to be deleted or timeout")
		return nil
	}

	klog.Warningf(logPfx+
		"Pod cannot be deleted within timeout %v, so force delete it",
		common.SecToDuration(timeoutSec))
	// Always confirm the force deletion to expose the failure that even force
	// deletion cannot delete the Pod, such as the Pod Finalizers is not empty.
	return c.deletePod(f, taskRoleName, taskIndex, pod.UID, true, true)
}

// Get Task's current Pod object, if not found, then clean up existing
// controlled Pod if any.
// Returned pod is either managed or nil, if it is the managed pod, it is not
// writable and may be outdated even if no error.
// Clean up instead of recovery is because the PodUID is always the ground truth.
func (c *FrameworkController) getOrCleanupPod(
	f *ci.Framework, cm *core.ConfigMap,
	taskRoleName string, taskIndex int32, confirm bool) (pod *core.Pod, err error) {
	logPfx := fmt.Sprintf("[%v][%v][%v]: getOrCleanupPod: ",
		f.Key(), taskRoleName, taskIndex)
	taskStatus := f.TaskStatus(taskRoleName, taskIndex)
	podName := taskStatus.PodName()

	if confirm {
		pod, err = c.kClient.CoreV1().Pods(f.Namespace).Get(podName,
			meta.GetOptions{})
	} else {
		pod, err = c.podLister.Pods(f.Namespace).Get(podName)
	}

	if err != nil {
		if apiErrors.IsNotFound(err) {
			return nil, nil
		} else {
			return nil, fmt.Errorf(logPfx+
				"Failed to get Pod %v: confirm: %v: %v",
				podName, confirm, err)
		}
	}

	if taskStatus.PodUID() == nil || *taskStatus.PodUID() != pod.UID {
		// pod is the unmanaged
		if meta.IsControlledBy(pod, cm) {
			// The managed Pod becomes unmanaged if and only if Framework.Status
			// is failed to persist due to FrameworkController restart or create fails
			// but succeeds on remote, so clean up the Pod to avoid unmanaged pod leak.
			klog.Warningf(logPfx+
				"Found unmanaged but controlled Pod, so explicitly delete it: %v, %v",
				pod.Name, pod.UID)
			if pod.DeletionTimestamp != nil {
				err = c.handlePodGracefulDeletion(f, taskRoleName, taskIndex, pod)
				if err != nil {
					return nil, err
				}
			}
			return nil, c.deletePod(f, taskRoleName, taskIndex, pod.UID, confirm, false)
		} else {
			// Do not own and manage the life cycle of not controlled object, so still
			// consider the get and controlled object clean up is success, and postpone
			// the potential naming conflict when creating the controlled object.
			klog.Warningf(logPfx+
				"Found unmanaged and uncontrolled Pod, and it may be naming conflict "+
				"with the controlled Pod to be created: %v, %v",
				pod.Name, pod.UID)
			return nil, nil
		}
	} else {
		// pod is the managed
		return pod, nil
	}
}

// Using UID to ensure we delete the right object.
// The podUID should be controlled by f's cm.
// Note, Pod force deletion can only be done after PodGracefulDeletionTimeoutSec
// expired which is mostly caused by bad node, for other cases, such as even if
// delete an unmanaged Pod, the force deletion may cause local node resource
// conflict since the node may be still healthy.
func (c *FrameworkController) deletePod(
	f *ci.Framework, taskRoleName string, taskIndex int32,
	podUID types.UID, confirm bool, force bool) error {
	podName := f.TaskStatus(taskRoleName, taskIndex).PodName()
	errPfx := fmt.Sprintf(
		"[%v][%v][%v]: Failed to delete Pod %v, %v: confirm: %v, force: %v: ",
		f.Key(), taskRoleName, taskIndex, podName, podUID, confirm, force)

	deleteOptions := &meta.DeleteOptions{Preconditions: &meta.Preconditions{UID: &podUID}}
	if force {
		deleteOptions.GracePeriodSeconds = common.PtrInt64(0)
	}
	deleteErr := c.kClient.CoreV1().Pods(f.Namespace).Delete(podName, deleteOptions)
	if deleteErr != nil {
		if !apiErrors.IsNotFound(deleteErr) {
			return fmt.Errorf(errPfx+"%v", deleteErr)
		}
	} else {
		if confirm {
			// Confirm it is deleted instead of still deleting.
			pod, getErr := c.kClient.CoreV1().Pods(f.Namespace).Get(podName,
				meta.GetOptions{})
			if getErr != nil {
				if !apiErrors.IsNotFound(getErr) {
					return fmt.Errorf(errPfx+
						"Pod cannot be got from remote: %v", getErr)
				}
			} else {
				if podUID == pod.UID {
					return fmt.Errorf(errPfx+
						"Pod with DeletionTimestamp %v still exist after deletion",
						pod.DeletionTimestamp)
				}
			}
		}
	}

	klog.Infof(
		"[%v][%v][%v]: Succeeded to delete Pod %v, %v: confirm: %v, force: %v",
		f.Key(), taskRoleName, taskIndex, podName, podUID, confirm, force)
	return nil
}

func (c *FrameworkController) createPod(
	f *ci.Framework, cm *core.ConfigMap,
	taskRoleName string, taskIndex int32) (*core.Pod, error) {
	pod := f.NewPod(cm, taskRoleName, taskIndex)
	errPfx := fmt.Sprintf(
		"[%v][%v][%v]: Failed to create Pod %v",
		f.Key(), taskRoleName, taskIndex, pod.Name)

	remotePod, createErr := c.kClient.CoreV1().Pods(f.Namespace).Create(pod)
	if createErr != nil {
		if apiErrors.IsAlreadyExists(createErr) {
			// Best effort to judge if conflict with a not controlled object.
			localPod, getErr := c.podLister.Pods(f.Namespace).Get(pod.Name)
			if getErr == nil && !meta.IsControlledBy(localPod, cm) {
				return nil, errorWrap.Wrapf(createErr, errPfx+": "+
					"Pod naming conflicts with others: "+
					"Existing Pod %v with DeletionTimestamp %v is not "+
					"controlled by current ConfigMap %v, %v",
					localPod.UID, localPod.DeletionTimestamp, cm.Name, cm.UID)
			}
		}

		return nil, errorWrap.Wrapf(createErr, errPfx)
	} else {
		klog.Infof(
			"[%v][%v][%v]: Succeeded to create Pod %v",
			f.Key(), taskRoleName, taskIndex, pod.Name)
		return remotePod, nil
	}
}

func (c *FrameworkController) completeTaskAttempt(
	f *ci.Framework, taskRoleName string, taskIndex int32,
	force bool, completionStatus *ci.TaskAttemptCompletionStatus) {
	logPfx := fmt.Sprintf(
		"[%v][%v][%v]: completeTaskAttempt: force: %v: ",
		f.Key(), taskRoleName, taskIndex, force)
	taskStatus := f.TaskStatus(taskRoleName, taskIndex)

	// CompletionStatus should be immutable after set.
	if taskStatus.AttemptStatus.CompletionStatus == nil {
		taskStatus.AttemptStatus.CompletionStatus = completionStatus
	}

	if force {
		f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskAttemptCompleted)

		if taskStatus.TaskAttemptInstanceUID() == nil {
			klog.Infof(logPfx+
				"TaskAttempt %v is completed with CompletionStatus: %v",
				taskStatus.TaskAttemptID(),
				common.ToJson(taskStatus.AttemptStatus.CompletionStatus))
		} else {
			klog.Infof(logPfx+
				"TaskAttemptInstance %v is completed with CompletionStatus: %v",
				*taskStatus.TaskAttemptInstanceUID(),
				common.ToJson(taskStatus.AttemptStatus.CompletionStatus))
		}

		// To ensure TaskAttemptCompleted is persisted before exposed its TaskAttempt,
		// we need to wait until next sync to expose the TaskAttempt, so manually
		// enqueue a sync.
		c.enqueueFrameworkSync(f, "TaskAttemptCompleted")
		klog.Infof(logPfx + "Waiting TaskAttemptCompleted to be persisted")
	} else {
		f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskAttemptDeletionPending)

		// To ensure TaskAttemptDeletionPending is persisted before deleting its pod,
		// we need to wait until next sync to delete the pod, so manually enqueue
		// a sync.
		c.enqueueFrameworkSync(f, "TaskAttemptDeletionPending")
		klog.Infof(logPfx + "Waiting TaskAttemptDeletionPending to be persisted")
	}
}

func (c *FrameworkController) completeFrameworkAttempt(
	f *ci.Framework, force bool, completionStatus *ci.FrameworkAttemptCompletionStatus) {
	logPfx := fmt.Sprintf(
		"[%v]: completeFrameworkAttempt: force: %v: ",
		f.Key(), force)

	// CompletionStatus should be immutable after set.
	if f.Status.AttemptStatus.CompletionStatus == nil {
		f.Status.AttemptStatus.CompletionStatus = completionStatus
	}

	for _, taskRoleStatus := range f.TaskRoleStatuses() {
		for _, taskStatus := range taskRoleStatus.TaskStatuses {
			if taskStatus.AttemptStatus.CompletionStatus == nil {
				taskStatus.AttemptStatus.CompletionStatus =
					ci.CompletionCodeFrameworkAttemptCompletion.
						NewTaskAttemptCompletionStatus(
							"Stop to complete current FrameworkAttempt", nil)
			}
		}
	}

	if force {
		for _, taskRoleStatus := range f.TaskRoleStatuses() {
			taskRoleName := taskRoleStatus.Name
			for _, taskStatus := range taskRoleStatus.TaskStatuses {
				taskIndex := taskStatus.Index
				if taskStatus.State != ci.TaskCompleted {
					if taskStatus.State != ci.TaskAttemptCompleted {
						c.completeTaskAttempt(f, taskRoleName, taskIndex, true, nil)
					}
					taskStatus.RetryPolicyStatus.RetryDelaySec = nil
					f.TransitionTaskState(taskRoleName, taskIndex, ci.TaskCompleted)
				}
			}
		}

		f.TransitionFrameworkState(ci.FrameworkAttemptCompleted)

		if f.FrameworkAttemptInstanceUID() == nil {
			klog.Infof(logPfx+
				"FrameworkAttempt %v is completed with CompletionStatus: %v",
				f.FrameworkAttemptID(),
				common.ToJson(f.Status.AttemptStatus.CompletionStatus))
		} else {
			klog.Infof(logPfx+
				"FrameworkAttemptInstance %v is completed with CompletionStatus: %v",
				*f.FrameworkAttemptInstanceUID(),
				common.ToJson(f.Status.AttemptStatus.CompletionStatus))
		}

		// To ensure FrameworkAttemptCompleted is persisted before exposed its
		// FrameworkAttempt, we need to wait until next sync to expose the
		// FrameworkAttempt, so manually enqueue a sync.
		c.enqueueFrameworkSync(f, "FrameworkAttemptCompleted")
		klog.Infof(logPfx + "Waiting FrameworkAttemptCompleted to be persisted")
	} else {
		f.TransitionFrameworkState(ci.FrameworkAttemptDeletionPending)

		// To ensure FrameworkAttemptDeletionPending is persisted before deleting
		// its cm, we need to wait until next sync to delete the cm, so manually
		// enqueue a sync.
		c.enqueueFrameworkSync(f, "FrameworkAttemptDeletionPending")
		klog.Infof(logPfx + "Waiting FrameworkAttemptDeletionPending to be persisted")
	}
}

// Best effort to compress and no need to requeue if failed, since the
// updateRemoteFrameworkStatus may still succeed if compress failed.
func (c *FrameworkController) compressFramework(f *ci.Framework) {
	if *c.cConfig.LargeFrameworkCompression {
		logPfx := fmt.Sprintf("[%v]: compressFramework: ", f.Key())
		klog.Infof(logPfx + "Started")
		defer func() { klog.Infof(logPfx + "Completed") }()

		err := f.Compress()
		if err != nil {
			klog.Warningf(logPfx+"Failed: %v", err)
		}
	}
}

func (c *FrameworkController) decompressFramework(f *ci.Framework) error {
	logPfx := fmt.Sprintf("[%v]: decompressFramework: ", f.Key())
	klog.Infof(logPfx + "Started")
	defer func() { klog.Infof(logPfx + "Completed") }()

	err := f.Decompress()
	if err != nil {
		return fmt.Errorf(logPfx+"Failed: %v", err)
	} else {
		return nil
	}
}

func (c *FrameworkController) updateRemoteFrameworkStatus(f *ci.Framework) error {
	logPfx := fmt.Sprintf("[%v]: updateRemoteFrameworkStatus: ", f.Key())
	klog.Infof(logPfx + "Started")
	defer func() { klog.Infof(logPfx + "Completed") }()

	tried := false
	updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var updateF *ci.Framework
		if !tried {
			// Using f to update optimistically, since f may not conflict with remote.
			updateF = f
			tried = true
		} else {
			// Only retry on conflict, so f must conflict with remote.
			// Try to resolve conflict by patching f.Status on more recent object.
			localF, getErr := c.fLister.Frameworks(f.Namespace).Get(f.Name)
			if getErr != nil {
				if apiErrors.IsNotFound(getErr) {
					return fmt.Errorf("Framework cannot be found in local cache: %v", getErr)
				} else {
					return fmt.Errorf("Framework cannot be got from local cache: %v", getErr)
				}
			} else {
				// Only resolve conflict for the same object to avoid updating another
				// object of the same name.
				if f.UID != localF.UID {
					return fmt.Errorf(
						"Framework UID mismatch: Current UID %v, Local Cached UID %v",
						f.UID, localF.UID)
				} else {
					updateF = localF.DeepCopy()
					updateF.Status = f.Status
				}
			}
		}

		_, updateErr := c.fClient.FrameworkcontrollerV1().Frameworks(updateF.Namespace).Update(updateF)
		return updateErr
	})

	if updateErr != nil {
		// Will still be requeued and retried after rate limited delay.
		return fmt.Errorf(logPfx+"Failed: %v", updateErr)
	} else {
		return nil
	}
}

func (c *FrameworkController) getExpectedFrameworkStatusInfo(key string) *ExpectedFrameworkStatusInfo {
	if value, ok := c.fExpectedStatusInfos.Load(key); ok {
		return value.(*ExpectedFrameworkStatusInfo)
	} else {
		return nil
	}
}

func (c *FrameworkController) deleteExpectedFrameworkStatusInfo(key string) {
	klog.Infof("[%v]: deleteExpectedFrameworkStatusInfo", key)
	c.fExpectedStatusInfos.Delete(key)
}

func (c *FrameworkController) updateExpectedFrameworkStatusInfo(key string,
	status *ci.FrameworkStatus, uid types.UID, remoteSynced bool) {
	klog.Infof(
		"[%v]: updateExpectedFrameworkStatusInfo: UID %v, RemoteSynced %v",
		key, uid, remoteSynced)
	c.fExpectedStatusInfos.Store(key, &ExpectedFrameworkStatusInfo{
		status:       status,
		uid:          uid,
		remoteSynced: remoteSynced,
	})
}

/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"time"
	"encoding/json"

	"github.com/golang/glog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	uruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/controller/history"
	"k8s.io/apimachinery/pkg/runtime"
	labelsutil "k8s.io/kubernetes/pkg/util/labels"
	hashutil "k8s.io/kubernetes/pkg/util/hash"
	"k8s.io/apimachinery/pkg/util/rand"


	samplev1alpha1 "k8s.io/sample-controller/pkg/apis/samplecontroller/v1alpha1"
	clientset "k8s.io/sample-controller/pkg/client/clientset/versioned"
	samplescheme "k8s.io/sample-controller/pkg/client/clientset/versioned/scheme"
	informers "k8s.io/sample-controller/pkg/client/informers/externalversions"
	listers "k8s.io/sample-controller/pkg/client/listers/samplecontroller/v1alpha1"
)

const controllerAgentName = "sample-controller"

const (
	// SuccessSynced is used as part of the Event 'reason' when a Foo is synced
	SuccessSynced = "Synced"
	// ErrResourceExists is used as part of the Event 'reason' when a Foo fails
	// to sync due to a Deployment of the same name already existing.
	ErrResourceExists = "ErrResourceExists"

	// MessageResourceExists is the message used for Events when a resource
	// fails to sync due to a Deployment already existing
	MessageResourceExists = "Resource %q already exists and is not managed by Foo"
	// MessageResourceSynced is the message used for an Event fired when a Foo
	// is synced successfully
	MessageResourceSynced = "Foo synced successfully"

	historyLimit = 5
)

var controllerKind = schema.GroupVersionKind{
					Group:   samplev1alpha1.SchemeGroupVersion.Group,
					Version: samplev1alpha1.SchemeGroupVersion.Version,
					Kind:    "Foo",
				}

// Controller is the controller implementation for Foo resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// sampleclientset is a clientset for our own API group
	sampleclientset clientset.Interface

	deploymentsLister appslisters.DeploymentLister
	deploymentsSynced cache.InformerSynced
	foosLister        listers.FooLister
	foosSynced        cache.InformerSynced

	controllerHistory history.Interface

	// workqueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workqueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder
}

// NewController returns a new sample controller
func NewController(
	kubeclientset kubernetes.Interface,
	sampleclientset clientset.Interface,
	kubeInformerFactory kubeinformers.SharedInformerFactory,
	sampleInformerFactory informers.SharedInformerFactory) *Controller {

	// obtain references to shared index informers for the Deployment and Foo
	// types.
	deploymentInformer := kubeInformerFactory.Apps().V1().Deployments()
	fooInformer := sampleInformerFactory.Samplecontroller().V1alpha1().Foos()

	// Create event broadcaster
	// Add sample-controller types to the default Kubernetes Scheme so Events can be
	// logged for sample-controller types.
	samplescheme.AddToScheme(scheme.Scheme)
	glog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset:     kubeclientset,
		sampleclientset:   sampleclientset,
		deploymentsLister: deploymentInformer.Lister(),
		deploymentsSynced: deploymentInformer.Informer().HasSynced,
		foosLister:        fooInformer.Lister(),
		foosSynced:        fooInformer.Informer().HasSynced,
		workqueue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Foos"),
		recorder:          recorder,
	}

	glog.Info("Setting up event handlers")
	// Set up an event handler for when Foo resources change
	fooInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueFoo,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueFoo(new)
		},
	})
	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer uruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting Foo controller")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.deploymentsSynced, c.foosSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers")
	// Launch two workers to process Foo resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			uruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Foo resource to be synced.
		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		glog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		uruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Foo resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		uruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the Foo resource with this namespace/name
	foo, err := c.foosLister.Foos(namespace).Get(name)
	if err != nil {
		// The Foo resource may no longer exist, in which case we stop
		// processing.
		if errors.IsNotFound(err) {
			uruntime.HandleError(fmt.Errorf("foo '%s' in work queue no longer exists", key))
			return nil
		}

		return err
	}

	glog.Info("Foo revisions: %v", foo)
		// list all revisions and sort them
	revisions, err := c.ListRevisions(foo)
	if err != nil {
		return err
	}
	history.SortControllerRevisions(revisions)

	// get the current, and update revisions
	currentRevision, updateRevision, collisionCount, err := c.getRevisions(foo, revisions)
	if err != nil {
		return err
	}

	// set the generation, and revisions in the returned status
	status := new(samplev1alpha1.FooStatus)
	status.ObservedGeneration = foo.Generation
	status.CurrentRevision = currentRevision.Name
	status.UpdateRevision = updateRevision.Name
	status.CollisionCount = &collisionCount

	glog.Info("Foo status %v", status)

	// update the set's status
	err = c.updateFooStatus(foo, status)
	if err != nil {
		return err
	}

	c.recorder.Event(foo, corev1.EventTypeNormal, SuccessSynced, MessageResourceSynced)
	// maintain the set's revision history limit
	return c.truncateHistory(foo, revisions, currentRevision, updateRevision)
}

func (c *Controller) updateFooStatus(foo *samplev1alpha1.Foo, status *samplev1alpha1.FooStatus) error {
		// complete any in progress rolling update if necessary
	completeRollingUpdate(foo, status)

	// if the status is not inconsistent do not perform an update
	if !inconsistentStatus(foo, status) {
		return nil
	}

	// NEVER modify objects from the store. It's a read-only, local cache.
	// You can use DeepCopy() to make a deep copy of original object and modify this copy
	// Or create a copy manually for better performance
	fooCopy := foo.DeepCopy()
	fooCopy.Status.CollisionCount = status.CollisionCount
	fooCopy.Status.CurrentRevision = status.CurrentRevision
	fooCopy.Status.ObservedGeneration = status.ObservedGeneration
	fooCopy.Status.UpdateRevision = status.UpdateRevision

	// Until #38113 is merged, we must use Update instead of UpdateStatus to
	// update the Status block of the Foo resource. UpdateStatus will not
	// allow changes to the Spec of the resource, which is ideal for ensuring
	// nothing other than resource status has been updated.
	_, err := c.sampleclientset.SamplecontrollerV1alpha1().Foos(foo.Namespace).Update(fooCopy)
	return err
}

// inconsistentStatus returns true if the ObservedGeneration of status is greater than set's
// Generation or if any of the status's fields do not match those of set's status.
func inconsistentStatus(foo *samplev1alpha1.Foo, status *samplev1alpha1.FooStatus) bool {
	return status.ObservedGeneration > foo.Status.ObservedGeneration ||
		status.CurrentRevision != foo.Status.CurrentRevision ||
		status.UpdateRevision != foo.Status.UpdateRevision
}

// enqueueFoo takes a Foo resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than Foo.
func (c *Controller) enqueueFoo(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		uruntime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

func computeHash(foo *samplev1alpha1.Foo, collisionCount *int32) uint32 {
	hasher := fnv.New32a()
	hashutil.DeepHashObject(hasher, foo)

	// Add collisionCount in the hash if it exists.
	if collisionCount != nil {
		collisionCountBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(collisionCountBytes, uint32(*collisionCount))
		hasher.Write(collisionCountBytes)
	}

	return hasher.Sum32()
}

// newRevision creates a new ControllerRevision containing a patch that reapplies the target state of set.
// The Revision of the returned ControllerRevision is set to revision. If the returned error is nil, the returned
// ControllerRevision is valid.
func newRevision(foo *samplev1alpha1.Foo, revision int64, collisionCount *int32) (*appsv1.ControllerRevision, error) {
	patch, err := getPatch(foo)
	if err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	hash := fmt.Sprint(computeHash(foo, collisionCount))
	name := foo.Name + "-" + rand.SafeEncodeString(hash)
	cr := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       foo.Namespace,
			Labels:          labelsutil.CloneAndAddLabel(foo.Labels, "fooHash", hash),
			Annotations:     foo.Annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(foo, controllerKind)},
		},
		Data:     runtime.RawExtension{Raw: patch},
		Revision: revision,
	}
	return cr, nil
}

var patchCodec = scheme.Codecs.LegacyCodec(appsv1.SchemeGroupVersion)

// getPatch returns a strategic merge patch that can be applied to restore Foo to a
// previous version.
func getPatch(foo *samplev1alpha1.Foo) ([]byte, error) {
	str, err := runtime.Encode(patchCodec, foo)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	json.Unmarshal([]byte(str), &raw)
	objCopy := make(map[string]interface{})
	spec := raw["spec"].(map[string]interface{})
	spec["$patch"] = "replace"
	objCopy["spec"] = spec
	patch, err := json.Marshal(objCopy)
	return patch, err
}

// ApplyRevision returns a new Foo constructed by restoring the state in revision to set. If the returned error
// is nil, the returned Foo is valid.
func ApplyRevision(foo *samplev1alpha1.Foo, revision *appsv1.ControllerRevision) (*samplev1alpha1.Foo, error) {
	clone := foo.DeepCopy()
	patched, err := strategicpatch.StrategicMergePatch([]byte(runtime.EncodeOrDie(patchCodec, clone)), revision.Data.Raw, clone)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(patched, clone)
	if err != nil {
		return nil, err
	}
	return clone, nil
}

// nextRevision finds the next valid revision number based on revisions. If the length of revisions
// is 0 this is 1. Otherwise, it is 1 greater than the largest revision's Revision. This method
// assumes that revisions has been sorted by Revision.
func nextRevision(revisions []*appsv1.ControllerRevision) int64 {
	count := len(revisions)
	if count <= 0 {
		return 1
	}
	return revisions[count-1].Revision + 1
}

// completeRollingUpdate completes a rolling update when all of set's replica Pods have been updated
// to the updateRevision. status's currentRevision is set to updateRevision and its' updateRevision
// is set to the empty string. status's currentReplicas is set to updateReplicas and its updateReplicas
// are set to 0.
func completeRollingUpdate(foo *samplev1alpha1.Foo, status *samplev1alpha1.FooStatus) {
	status.CurrentRevision = status.UpdateRevision
}

func (c *Controller) ListRevisions(foo *samplev1alpha1.Foo) ([]*appsv1.ControllerRevision, error) {
	return c.controllerHistory.ListControllerRevisions(foo, nil)
}

func (c *Controller) AdoptOrphanRevisions(
	foo *samplev1alpha1.Foo,
	revisions []*appsv1.ControllerRevision) error {
	for i := range revisions {
		adopted, err := c.controllerHistory.AdoptControllerRevision(foo, controllerKind, revisions[i])
		if err != nil {
			return err
		}
		revisions[i] = adopted
	}
	return nil
}

// truncateHistory truncates any non-live ControllerRevisions in revisions from set's history.
func (c *Controller) truncateHistory(
	foo *samplev1alpha1.Foo,
	revisions []*appsv1.ControllerRevision,
	current *appsv1.ControllerRevision,
	update *appsv1.ControllerRevision) error {
	history := make([]*appsv1.ControllerRevision, 0, len(revisions))
	for i := range revisions {
		history = append(history, revisions[i])
	}
	historyLen := len(history)
	if historyLen <= historyLimit {
		return nil
	}
	// delete any non-live history to maintain the revision limit.
	history = history[:(historyLen - historyLimit)]
	for i := 0; i < len(history); i++ {
		if err := c.controllerHistory.DeleteControllerRevision(history[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) getRevisions(
	foo *samplev1alpha1.Foo,
	revisions []*appsv1.ControllerRevision) (*appsv1.ControllerRevision, *appsv1.ControllerRevision, int32, error) {
	var currentRevision, updateRevision *appsv1.ControllerRevision

	revisionCount := len(revisions)
	history.SortControllerRevisions(revisions)

	// Use a local copy of set.Status.CollisionCount to avoid modifying set.Status directly.
	// This copy is returned so the value gets carried over to set.Status in updateStatefulSet.
	var collisionCount int32
	if foo.Status.CollisionCount != nil {
		collisionCount = *foo.Status.CollisionCount
	}

	// create a new revision from the current set
	updateRevision, err := newRevision(foo, nextRevision(revisions), &collisionCount)
	if err != nil {
		return nil, nil, collisionCount, err
	}

	// find any equivalent revisions
	equalRevisions := history.FindEqualRevisions(revisions, updateRevision)
	equalCount := len(equalRevisions)

	if equalCount > 0 && history.EqualRevision(revisions[revisionCount-1], equalRevisions[equalCount-1]) {
		// if the equivalent revision is immediately prior the update revision has not changed
		updateRevision = revisions[revisionCount-1]
	} else if equalCount > 0 {
		// if the equivalent revision is not immediately prior we will roll back by incrementing the
		// Revision of the equivalent revision
		updateRevision, err = c.controllerHistory.UpdateControllerRevision(
			equalRevisions[equalCount-1],
			updateRevision.Revision)
		if err != nil {
			return nil, nil, collisionCount, err
		}
	} else {
		//if there is no equivalent revision we create a new one
		updateRevision, err = c.controllerHistory.CreateControllerRevision(foo, updateRevision, &collisionCount)
		if err != nil {
			return nil, nil, collisionCount, err
		}
	}

	// attempt to find the revision that corresponds to the current revision
	for i := range revisions {
		if revisions[i].Name == foo.Status.CurrentRevision {
			currentRevision = revisions[i]
		}
	}

	// if the current revision is nil we initialize the history by setting it to the update revision
	if currentRevision == nil {
		currentRevision = updateRevision
	}

	return currentRevision, updateRevision, collisionCount, nil
}

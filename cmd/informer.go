package main

import (
	"context"
	"fmt"

	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/restmapper"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	dynamic "k8s.io/client-go/deprecated-dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

//InformerOpts type
type InformerOpts struct {
	Handler     func(ctx context.Context, event EventType, obj *unstructured.Unstructured, numRetries int) error
	MaxRetries  int
	RateLimiter workqueue.RateLimiter
}

//EventType type
type EventType string

const (
	//EventAdd constant
	EventAdd EventType = "add"
	//EventUpdate constant
	EventUpdate EventType = "update"
	//EventDelete constant
	EventDelete EventType = "delete"
)

type informer struct {
	InformerOpts
	queue          workqueue.RateLimitingInterface
	deletedObjects objectMap
	watches        informerWatchList
	kubeConfig     *rest.Config
	clientPool     dynamic.ClientPool
	restMapper     *restmapper.DeferredDiscoveryRESTMapper
}
type informerWatch struct {
	name     string
	informer *informer
	index    int
	watcher  cache.SharedIndexInformer
}

type informerWatchList []*informerWatch

type objectKey struct {
	watchIndex int
	key        string
}

type eventKey struct {
	objectKey
	event EventType
}

type objectMap map[objectKey]*unstructured.Unstructured

//NewInformer func
func NewInformer(kubeConfig *rest.Config, opts InformerOpts) Informer {
	kubeClient := clientset.NewForConfigOrDie(kubeConfig)
	cachedDiscoveryClient := cached.NewMemCacheClient(kubeClient.Discovery())
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscoveryClient)
	restMapper.Reset()
	kubeConfig.ContentConfig = dynamic.ContentConfig()
	return &informer{
		InformerOpts:   opts,
		queue:          workqueue.NewRateLimitingQueue(opts.RateLimiter),
		deletedObjects: objectMap{},
		watches:        informerWatchList{},
		kubeConfig:     kubeConfig,
		clientPool:     dynamic.NewClientPool(kubeConfig, restMapper, dynamic.LegacyAPIPathResolverFunc),
		restMapper:     restMapper,
	}
}

//Informer interface
type Informer interface {
	Watch(apiVersion string, kind string, namespace string, selector string, resync time.Duration) error
	Run(ctx context.Context)
}

func (i *informer) getResourceClient(apiVersion, kind, namespace string) (dynamic.ResourceInterface, string, string, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to parse apiVersion: %v", err)
	}
	gvk := schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    kind,
	}
	client, err := i.clientPool.ClientForGroupVersionKind(gvk)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to get client for GroupVersionKind(%s): %v", gvk.String(), err)
	}
	resource, err := apiResource(gvk, i.restMapper)
	if err != nil {
		return nil, "", "", fmt.Errorf("failed to get resource type: %v", err)
	}
	if !resource.Namespaced {
		namespace = metav1.NamespaceAll
	}
	return client.Resource(resource, namespace), resource.Name, namespace, nil
}

// apiResource consults the REST mapper to translate an <apiVersion, kind, namespace> tuple to a metav1.APIResource struct.
func apiResource(gvk schema.GroupVersionKind, restMapper *restmapper.DeferredDiscoveryRESTMapper) (*metav1.APIResource, error) {
	mapping, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("failed to get the resource REST mapping for GroupVersionKind(%s): %v", gvk.String(), err)
	}
	resource := &metav1.APIResource{
		Name:       mapping.Resource.Resource,
		Namespaced: mapping.Scope == meta.RESTScopeNamespace,
		Kind:       gvk.Kind,
	}
	return resource, nil
}

func (i *informer) Watch(apiVersion string, kind string, namespace string, selector string, resync time.Duration) error {
	resourceClient, resourcePluralName, namespace, err := i.getResourceClient(apiVersion, kind, namespace)
	if err != nil {
		return err
	}
	watch := &informerWatch{
		name:     fmt.Sprintf("%s/%s %s", namespace, resourcePluralName, selector),
		informer: i,
		index:    len(i.watches),
		watcher: cache.NewSharedIndexInformer(
			newListWatcherFromResourceClient(resourceClient, selector),
			&unstructured.Unstructured{},
			resync,
			cache.Indexers{},
		),
	}
	watch.watcher.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    watch.handleAdd,
		DeleteFunc: watch.handleDelete,
		UpdateFunc: watch.handleUpdate,
	})
	i.watches = append(i.watches, watch)
	return nil
}

func newListWatcherFromResourceClient(resourceClient dynamic.ResourceInterface, labelSelector string) *cache.ListWatch {
	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		if labelSelector != "" {
			options.LabelSelector = labelSelector
		}
		return resourceClient.List(options)
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		if labelSelector != "" {
			options.LabelSelector = labelSelector
		}
		return resourceClient.Watch(options)
	}
	return &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}
}

func (i *informer) Run(ctx context.Context) {
	defer i.queue.ShutDown()
	for _, watch := range i.watches {
		logger.Printf("watching %s", watch.name)
		go watch.watcher.Run(ctx.Done())
	}
	for _, watch := range i.watches {
		if !cache.WaitForCacheSync(ctx.Done(), watch.watcher.HasSynced) {
			panic("Timed out waiting for caches to sync")
		}
	}
	go wait.Until(func() {
		for i.processNextItem(ctx) {
		}
	}, time.Second, ctx.Done())

	<-ctx.Done()
	logger.Printf("stopped all watch")
}

func (w *informerWatch) handleAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		panic(err)
	}
	w.informer.queue.Add(eventKey{objectKey{w.index, key}, EventAdd})
}

func (w *informerWatch) handleDelete(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		panic(err)
	}

	w.informer.deletedObjects[objectKey{w.index, key}] = obj.(*unstructured.Unstructured).DeepCopy()
	w.informer.queue.Add(eventKey{objectKey{w.index, key}, EventDelete})
}

func (w *informerWatch) handleUpdate(oldObj, newObj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err != nil {
		panic(err)
	}
	w.informer.queue.Add(eventKey{objectKey{w.index, key}, EventUpdate})
}

func (i *informer) processNextItem(ctx context.Context) bool {
	item, quit := i.queue.Get()
	if quit {
		return false
	}
	defer i.queue.Done(item)
	eventKey, numRetries := item.(eventKey), i.queue.NumRequeues(item)
	watcher := i.watches[eventKey.watchIndex].watcher
	obj, exists, err := watcher.GetIndexer().GetByKey(eventKey.key)
	if err == nil {
		if !exists {
			if _, ok := i.deletedObjects[eventKey.objectKey]; !ok {
				logger.Printf("no last known state found for (%v)", eventKey)
				i.queue.Forget(item)
				return true
			}
			err = i.Handler(ctx, EventDelete, i.deletedObjects[eventKey.objectKey], numRetries)
		} else {
			err = i.Handler(ctx, eventKey.event, obj.(*unstructured.Unstructured).DeepCopy(), numRetries)
		}
	}
	if err != nil {
		logger.Printf("error processing (%v, retries %v/%v): %v", eventKey, numRetries, i.MaxRetries, err)
		if i.MaxRetries < 0 || numRetries < i.MaxRetries {
			i.queue.AddRateLimited(item)
			return true
		}
	}
	if !exists {
		delete(i.deletedObjects, eventKey.objectKey)
	}
	i.queue.Forget(item)
	return true
}

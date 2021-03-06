/*
Copyright 2018 The Kubernetes Authors.

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

// Modified from the original source (available at
// https://github.com/kubernetes-sigs/controller-runtime/tree/v0.5.0/pkg/cache)

package dynamiccache

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/open-policy-agent/gatekeeper/third_party/sigs.k8s.io/controller-runtime/pkg/dynamiccache/internal"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

var (
	_ cache.Informers = &dynamicInformerCache{}
	_ client.Reader   = &dynamicInformerCache{}
	_ cache.Cache     = &dynamicInformerCache{}
)

// ErrCacheNotStarted is returned when trying to read from the cache that wasn't started.
type ErrCacheNotStarted struct{}

func (*ErrCacheNotStarted) Error() string {
	return "the cache is not started, can not read objects"
}

// dynamicInformerCache is a Kubernetes Object cache populated from InformersMap.  dynamicInformerCache wraps an InformersMap.
type dynamicInformerCache struct {
	*internal.InformersMap
}

// Get implements Reader
func (ip *dynamicInformerCache) Get(ctx context.Context, key client.ObjectKey, out runtime.Object) error {
	gvk, err := apiutil.GVKForObject(out, ip.Scheme)
	if err != nil {
		return err
	}

	started, cache, err := ip.InformersMap.Get(gvk, out)
	if err != nil {
		return err
	}

	if !started {
		return &ErrCacheNotStarted{}
	}
	return cache.Reader.Get(ctx, key, out)
}

// List implements Reader
func (ip *dynamicInformerCache) List(ctx context.Context, out runtime.Object, opts ...client.ListOption) error {
	gvk, err := apiutil.GVKForObject(out, ip.Scheme)
	if err != nil {
		return err
	}

	if !strings.HasSuffix(gvk.Kind, "List") {
		return fmt.Errorf("non-list type %T (kind %q) passed as output", out, gvk)
	}
	// we need the non-list GVK, so chop off the "List" from the end of the kind
	gvk.Kind = gvk.Kind[:len(gvk.Kind)-4]
	_, isUnstructured := out.(*unstructured.UnstructuredList)
	var cacheTypeObj runtime.Object
	if isUnstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		cacheTypeObj = u
	} else {
		itemsPtr, err := apimeta.GetItemsPtr(out)
		if err != nil {
			return nil
		}
		// http://knowyourmeme.com/memes/this-is-fine
		elemType := reflect.Indirect(reflect.ValueOf(itemsPtr)).Type().Elem()
		cacheTypeValue := reflect.Zero(reflect.PtrTo(elemType))
		var ok bool
		cacheTypeObj, ok = cacheTypeValue.Interface().(runtime.Object)
		if !ok {
			return fmt.Errorf("cannot get cache for %T, its element %T is not a runtime.Object", out, cacheTypeValue.Interface())
		}
	}

	started, cache, err := ip.InformersMap.Get(gvk, cacheTypeObj)
	if err != nil {
		return err
	}

	if !started {
		return &ErrCacheNotStarted{}
	}

	return cache.Reader.List(ctx, out, opts...)
}

// GetInformerForKind returns the informer for the GroupVersionKind
func (ip *dynamicInformerCache) GetInformerForKind(gvk schema.GroupVersionKind) (cache.Informer, error) {
	// Map the gvk to an object
	obj, err := ip.Scheme.New(gvk)
	if err != nil {
		return nil, err
	}
	_, i, err := ip.InformersMap.Get(gvk, obj)
	if err != nil {
		return nil, err
	}
	return i.Informer, err
}

// GetInformer returns the informer for the obj
func (ip *dynamicInformerCache) GetInformer(obj runtime.Object) (cache.Informer, error) {
	gvk, err := apiutil.GVKForObject(obj, ip.Scheme)
	if err != nil {
		return nil, err
	}
	_, i, err := ip.InformersMap.Get(gvk, obj)
	if err != nil {
		return nil, err
	}
	return i.Informer, err
}

// GetInformerNonBlocking returns the informer for the obj without waiting for its cache to sync.
func (ip *dynamicInformerCache) GetInformerNonBlocking(obj runtime.Object) (cache.Informer, error) {
	gvk, err := apiutil.GVKForObject(obj, ip.Scheme)
	if err != nil {
		return nil, err
	}
	_, i, err := ip.InformersMap.GetNonBlocking(gvk, obj)
	if err != nil {
		return nil, err
	}
	return i.Informer, err
}

// NeedLeaderElection implements the LeaderElectionRunnable interface
// to indicate that this can be started without requiring the leader lock
func (ip *dynamicInformerCache) NeedLeaderElection() bool {
	return false
}

// IndexField adds an indexer to the underlying cache, using extraction function to get
// value(s) from the given field.  This index can then be used by passing a field selector
// to List. For one-to-one compatibility with "normal" field selectors, only return one value.
// The values may be anything.  They will automatically be prefixed with the namespace of the
// given object, if present.  The objects passed are guaranteed to be objects of the correct type.
func (ip *dynamicInformerCache) IndexField(obj runtime.Object, field string, extractValue client.IndexerFunc) error {
	informer, err := ip.GetInformer(obj)
	if err != nil {
		return err
	}
	return indexByField(informer, field, extractValue)
}

func indexByField(indexer cache.Informer, field string, extractor client.IndexerFunc) error {
	indexFunc := func(objRaw interface{}) ([]string, error) {
		// TODO(directxman12): check if this is the correct type?
		obj, isObj := objRaw.(runtime.Object)
		if !isObj {
			return nil, fmt.Errorf("object of type %T is not an Object", objRaw)
		}
		meta, err := apimeta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		ns := meta.GetNamespace()

		rawVals := extractor(obj)
		var vals []string
		if ns == "" {
			// if we're not doubling the keys for the namespaced case, just re-use what was returned to us
			vals = rawVals
		} else {
			// if we need to add non-namespaced versions too, double the length
			vals = make([]string, len(rawVals)*2)
		}
		for i, rawVal := range rawVals {
			// save a namespaced variant, so that we can ask
			// "what are all the object matching a given index *in a given namespace*"
			vals[i] = internal.KeyToNamespacedKey(ns, rawVal)
			if ns != "" {
				// if we have a namespace, also inject a special index key for listing
				// regardless of the object namespace
				vals[i+len(rawVals)] = internal.KeyToNamespacedKey("", rawVal)
			}
		}

		return vals, nil
	}

	return indexer.AddIndexers(toolscache.Indexers{internal.FieldIndexName(field): indexFunc})
}

// Remove removes an informer specified by the obj argument from the cache and stops it if it existed.
func (ip *dynamicInformerCache) Remove(obj runtime.Object) error {
	gvk, err := apiutil.GVKForObject(obj, ip.Scheme)
	if err != nil {
		return err
	}

	ip.InformersMap.Remove(gvk, obj)
	return nil
}

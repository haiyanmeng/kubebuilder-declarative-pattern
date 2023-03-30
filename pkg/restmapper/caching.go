package restmapper

import (
	"context"
	"fmt"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// cache is our cache of schema information.
type cache struct {
	mutex         sync.Mutex
	groups        map[string]metav1.APIGroup
	groupVersions map[schema.GroupVersion]*cachedGroupVersion
}

// newCache is the constructor for a cache.
func newCache() *cache {
	return &cache{
		groupVersions: make(map[schema.GroupVersion]*cachedGroupVersion),
	}
}

// findGroupInfo returns the APIGroup for the specified group, querying discovery if not cached.
// If not found, returns APIGroup{}, false, nil
func (c *cache) findGroupInfo(ctx context.Context, discovery discovery.DiscoveryInterface, groupName string) (metav1.APIGroup, bool, error) {
	log := log.FromContext(ctx)

	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.groups == nil {
		log.Info("discovering server groups")
		serverGroups, err := discovery.ServerGroups()
		if err != nil {
			klog.Infof("unexpected error from ServerGroups: %v", err)
			return metav1.APIGroup{}, false, fmt.Errorf("error from ServerGroups: %w", err)
		}

		groups := make(map[string]metav1.APIGroup)
		for i := range serverGroups.Groups {
			group := &serverGroups.Groups[i]
			groups[group.Name] = *group
		}
		c.groups = groups
	}

	group, found := c.groups[groupName]
	return group, found, nil
}

// cachedGroupVersion caches (all) the resource information for a particular groupversion.
type cachedGroupVersion struct {
	gv    schema.GroupVersion
	mutex sync.Mutex
	kinds map[string]cachedGVR
	// resource to kind
	toKind map[string]string
}

// cachedGVR caches the information for a particular resource.
type cachedGVR struct {
	Resource string
	Scope    meta.RESTScope
}

// KindFromGVR finds out the Kind from the GVR in the cache. If the GVR version is not given, we will iterate all the matching
// GR in the cache and return the first matching one.
// e.g. https://github.com/kubernetes/kubernetes/blob/a0cff30104ea950a5cc733a109e7f9084275e49e/staging/src/k8s.io/kubectl/pkg/cmd/apply/applyset.go#L353
func (c *cache) KindFromGVR(gvr schema.GroupVersionResource) string {
	if gvr.Version != "" {
		cachedgvr, ok := c.groupVersions[gvr.GroupVersion()]
		if !ok {
			return ""
		}
		return cachedgvr.toKind[gvr.Resource]
	}
	for keyGVR, cachedgvr := range c.groupVersions {
		if keyGVR.Group != gvr.Group {
			continue
		}
		kind, ok := cachedgvr.toKind[gvr.Resource]
		if ok && kind != "" {
			return kind
		}
	}
	return ""
}

// findRESTMapping returns the RESTMapping for the specified GVK, querying discovery if not cached.
func (c *cache) findRESTMapping(ctx context.Context, discovery discovery.DiscoveryInterface, gv schema.GroupVersion, kind string) (*meta.RESTMapping, error) {
	c.mutex.Lock()
	cached := c.groupVersions[gv]
	if cached == nil {
		cached = &cachedGroupVersion{gv: gv, toKind: make(map[string]string)}
		c.groupVersions[gv] = cached
	}
	c.mutex.Unlock()
	return cached.findRESTMapping(ctx, discovery, kind)
}

// findRESTMapping returns the RESTMapping for the specified GVK, querying discovery if not cached.
func (c *cachedGroupVersion) findRESTMapping(ctx context.Context, discovery discovery.DiscoveryInterface, kind string) (*meta.RESTMapping, error) {
	kinds, err := c.fetch(ctx, discovery)
	if err != nil {
		return nil, err
	}

	cached, found := kinds[kind]
	if !found {
		return nil, nil
	}
	return &meta.RESTMapping{
		Resource:         c.gv.WithResource(cached.Resource),
		GroupVersionKind: c.gv.WithKind(kind),
		Scope:            cached.Scope,
	}, nil
}

// fetch returns the metadata, fetching it if not cached.
func (c *cachedGroupVersion) fetch(ctx context.Context, discovery discovery.DiscoveryInterface) (map[string]cachedGVR, error) {
	log := log.FromContext(ctx)

	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.kinds != nil {
		return c.kinds, nil
	}

	log.Info("discovering server resources for group/version", "gv", c.gv.String())
	resourceList, err := discovery.ServerResourcesForGroupVersion(c.gv.String())
	if err != nil {
		// We treat "no match" as an empty result, but any other error percolates back up
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return nil, nil
		} else {
			klog.Infof("unexpected error from ServerResourcesForGroupVersion(%v): %v", c.gv, err)
			return nil, fmt.Errorf("error from ServerResourcesForGroupVersion(%v): %w", c.gv, err)
		}
	}

	kinds := make(map[string]cachedGVR)
	for i := range resourceList.APIResources {
		resource := resourceList.APIResources[i]

		// if we have a slash, then this is a subresource and we shouldn't create mappings for those.
		if strings.Contains(resource.Name, "/") {
			continue
		}

		scope := meta.RESTScopeRoot
		if resource.Namespaced {
			scope = meta.RESTScopeNamespace
		}
		kinds[resource.Kind] = cachedGVR{
			Resource: resource.Name,
			Scope:    scope,
		}
		c.toKind[resource.Name] = resource.Kind
	}
	c.kinds = kinds
	return kinds, nil
}

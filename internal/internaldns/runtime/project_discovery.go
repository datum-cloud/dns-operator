package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"sync"

	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kubeadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type directoryProject struct {
	projectRuntime
	cancel context.CancelFunc
}

type projectDirectory struct {
	mu       sync.RWMutex
	projects map[string]directoryProject
	discover client.Reader
}

func newProjectDirectory(projects []projectRuntime) *projectDirectory {
	d := &projectDirectory{projects: map[string]directoryProject{}}
	for _, p := range projects {
		d.projects[p.config.Name] = directoryProject{projectRuntime: p}
	}
	return d
}

func (d *projectDirectory) scopes() []platform.ProjectScope {
	d.mu.RLock()
	defer d.mu.RUnlock()
	scopes := make([]platform.ProjectScope, 0, len(d.projects))
	names := make([]string, 0, len(d.projects))
	for name := range d.projects {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := d.projects[name]
		scopes = append(scopes, platform.ProjectScope{Client: p.direct, Namespace: p.config.Namespace, RequestTimeout: p.config.KubeAPI.requestTimeout()})
	}
	return scopes
}

func (d *projectDirectory) selectProject(ctx context.Context, req kubeadmission.Request) (client.Client, error) {
	parents := req.UserInfo.Extra[parentNameExtra]
	d.mu.RLock()
	if d.discover == nil {
		projects := make([]projectRuntime, 0, len(d.projects))
		for _, p := range d.projects {
			projects = append(projects, p.projectRuntime)
		}
		d.mu.RUnlock()
		return projectSelector(projects)(ctx, req)
	}
	reader := d.discover
	d.mu.RUnlock()
	if len(parents) != 1 || req.Namespace == "" {
		return nil, errors.New("authenticated project parent is missing or ambiguous")
	}
	var project resourcemanagerv1alpha1.Project
	if err := reader.Get(ctx, client.ObjectKey{Name: parents[0]}, &project); err != nil {
		return nil, fmt.Errorf("observe authenticated project: %w", err)
	}
	if !project.DeletionTimestamp.IsZero() || !apimeta.IsStatusConditionTrue(project.Status.Conditions, resourcemanagerv1alpha1.ProjectReady) {
		return nil, errors.New("authenticated project is unavailable")
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, found := d.projects[parents[0]]
	if !found || p.config.ProjectUID != project.UID {
		return nil, errors.New("authenticated project source is not ready")
	}
	return p.direct, nil
}

type projectDiscovery struct {
	client      client.Client
	directory   *projectDirectory
	rest        *rest.Config
	kubeAPI     KubeAPIClientConfig
	makeProject func(ProjectConfig, *rest.Config) (projectRuntime, error)
}

func (d *projectDiscovery) step(ctx context.Context) error {
	d.directory.mu.Lock()
	d.directory.discover = d.client
	d.directory.mu.Unlock()
	var projects resourcemanagerv1alpha1.ProjectList
	if err := d.client.List(ctx, &projects); err != nil {
		return err
	}
	seen := map[string]bool{}
	var failures error
	for i := range projects.Items {
		p := &projects.Items[i]
		seen[p.Name] = true
		if p.UID == "" {
			failures = errors.Join(failures, fmt.Errorf("project %s has no identity", p.Name))
			continue
		}
		d.directory.mu.RLock()
		existing, exists := d.directory.projects[p.Name]
		d.directory.mu.RUnlock()
		ready := p.DeletionTimestamp.IsZero() && apimeta.IsStatusConditionTrue(p.Status.Conditions, resourcemanagerv1alpha1.ProjectReady)
		if exists && (existing.config.ProjectUID != p.UID || !ready) {
			d.remove(p.Name)
			exists = false
		}
		if !ready || exists {
			continue
		}
		projectREST, err := projectRESTConfig(d.rest, p.Name)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		worker, err := d.makeProject(ProjectConfig{Name: p.Name, ProjectUID: p.UID, KubeAPI: d.kubeAPI}, projectREST)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		sourceCtx, cancel := context.WithCancel(ctx)
		d.directory.mu.Lock()
		d.directory.projects[p.Name] = directoryProject{projectRuntime: worker, cancel: cancel}
		d.directory.mu.Unlock()
		go runProjectSource(sourceCtx, worker)
	}
	d.directory.mu.RLock()
	names := make([]string, 0, len(d.directory.projects))
	for name := range d.directory.projects {
		names = append(names, name)
	}
	d.directory.mu.RUnlock()
	for _, name := range names {
		if !seen[name] {
			d.remove(name)
		}
	}
	return failures
}

func (d *projectDiscovery) remove(name string) {
	d.directory.mu.Lock()
	defer d.directory.mu.Unlock()
	if p, found := d.directory.projects[name]; found {
		if p.cancel != nil {
			p.cancel()
		}
		delete(d.directory.projects, name)
	}
}

func projectRESTConfig(base *rest.Config, name string) (*rest.Config, error) {
	endpoint, err := url.Parse(base.Host)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, fmt.Errorf("invalid project discovery API endpoint %q", base.Host)
	}
	endpoint.Path = path.Join(endpoint.Path, "apis/resourcemanager.miloapis.com/v1alpha1/projects", name, "control-plane")
	config := rest.CopyConfig(base)
	config.Host = endpoint.String()
	return config, nil
}

func projectCache(namespace string) cache.Options {
	if namespace == "" {
		return cache.Options{}
	}
	return cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}
}

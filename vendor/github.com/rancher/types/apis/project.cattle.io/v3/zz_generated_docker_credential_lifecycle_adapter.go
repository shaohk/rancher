package v3

import (
	"github.com/rancher/norman/lifecycle"
	"k8s.io/apimachinery/pkg/runtime"
)

type DockerCredentialLifecycle interface {
	Create(obj *DockerCredential) (*DockerCredential, error)
	Remove(obj *DockerCredential) (*DockerCredential, error)
	Updated(obj *DockerCredential) (*DockerCredential, error)
}

type dockerCredentialLifecycleAdapter struct {
	lifecycle DockerCredentialLifecycle
}

func (w *dockerCredentialLifecycleAdapter) Create(obj runtime.Object) (runtime.Object, error) {
	o, err := w.lifecycle.Create(obj.(*DockerCredential))
	if o == nil {
		return nil, err
	}
	return o, err
}

func (w *dockerCredentialLifecycleAdapter) Finalize(obj runtime.Object) (runtime.Object, error) {
	o, err := w.lifecycle.Remove(obj.(*DockerCredential))
	if o == nil {
		return nil, err
	}
	return o, err
}

func (w *dockerCredentialLifecycleAdapter) Updated(obj runtime.Object) (runtime.Object, error) {
	o, err := w.lifecycle.Updated(obj.(*DockerCredential))
	if o == nil {
		return nil, err
	}
	return o, err
}

func NewDockerCredentialLifecycleAdapter(name string, clusterScoped bool, client DockerCredentialInterface, l DockerCredentialLifecycle) DockerCredentialHandlerFunc {
	adapter := &dockerCredentialLifecycleAdapter{lifecycle: l}
	syncFn := lifecycle.NewObjectLifecycleAdapter(name, clusterScoped, adapter, client.ObjectClient())
	return func(key string, obj *DockerCredential) (*DockerCredential, error) {
		newObj, err := syncFn(key, obj)
		if o, ok := newObj.(*DockerCredential); ok {
			return o, err
		}
		return nil, err
	}
}

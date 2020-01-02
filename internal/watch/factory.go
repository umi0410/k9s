package watch

import (
	"fmt"
	"time"

	"github.com/derailed/k9s/internal/client"
	"github.com/rs/zerolog/log"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	di "k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
)

const (
	defaultResync = 10 * time.Minute
	allNamespaces = ""
	clusterScope  = "-"
)

// ReadVerbs lists out RO verbs.
var ReadVerbs = []string{"get", "list", "watch"}

// Factory tracks various resource informers.
type Factory struct {
	factories  map[string]di.DynamicSharedInformerFactory
	client     client.Connection
	stopChan   chan struct{}
	forwarders Forwarders
}

// NewFactory returns a new informers factory.
func NewFactory(client client.Connection) *Factory {
	return &Factory{
		client:     client,
		factories:  make(map[string]di.DynamicSharedInformerFactory),
		forwarders: NewForwarders(),
	}
}

// Start initializes the informers until caller cancels the context.
func (f *Factory) Start(ns string) {
	log.Debug().Msgf("Factory START with ns `%q", ns)
	f.stopChan = make(chan struct{})
	for ns, fac := range f.factories {
		log.Debug().Msgf("Starting factory in ns %q", ns)
		fac.Start(f.stopChan)
	}
}

// Terminate terminates all watchers and forwards.
func (f *Factory) Terminate() {
	if f.stopChan != nil {
		close(f.stopChan)
		f.stopChan = nil
	}
	for k := range f.factories {
		delete(f.factories, k)
	}
	f.forwarders.DeleteAll()
}

// List returns a resource collection.
func (f *Factory) List(gvr, ns string, wait bool, sel labels.Selector) ([]runtime.Object, error) {
	defer func(t time.Time) {
		log.Debug().Msgf("LIST elapsed %v", time.Since(t))
	}(time.Now())

	Dump(f)
	log.Debug().Msgf("List %q:%q", ns, gvr)
	inf, err := f.CanForResource(ns, gvr, []string{"list", "watch"})
	if err != nil {
		return nil, err
	}
	if ns == clusterScope {
		ns = allNamespaces
	}

	if wait {
		f.waitForCacheSync(ns)
	}
	return inf.Lister().ByNamespace(ns).List(sel)
}

// Get retrieves a given resource.
func (f *Factory) Get(gvr, path string, wait bool, sel labels.Selector) (runtime.Object, error) {
	defer func(t time.Time) {
		log.Debug().Msgf("GET elapsed %v", time.Since(t))
	}(time.Now())

	ns, n := namespaced(path)
	inf, err := f.CanForResource(ns, gvr, []string{"get"})
	if err != nil {
		return nil, err
	}
	if ns == clusterScope {
		ns = allNamespaces
	}

	if wait {
		f.waitForCacheSync(ns)
	}
	return inf.Lister().ByNamespace(ns).Get(n)
}

func (f *Factory) waitForCacheSync(ns string) {
	if fac, ok := f.factories[ns]; ok {
		// Hang for a sec for the cache to refresh if still not done bail out!
		const dur = 1 * time.Second
		c := make(chan struct{})
		go func(c chan struct{}) {
			<-time.After(dur)
			log.Debug().Msgf("Wait for sync timed out!")
			close(c)
		}(c)
		fac.WaitForCacheSync(c)
		log.Debug().Msgf("Sync completed for ns %q", ns)
	}
}

// WaitForCacheSync waits for all factories to update their cache.
func (f *Factory) WaitForCacheSync() {
	for ns, fac := range f.factories {
		m := fac.WaitForCacheSync(f.stopChan)
		for k, v := range m {
			log.Debug().Msgf("CACHE `%q Loaded %t:%s", ns, v, k)
		}
	}
}

// Client return the factory connection.
func (f *Factory) Client() client.Connection {
	return f.client
}

// FactoryFor returns a factory for a given namespace.
func (f *Factory) FactoryFor(ns string) di.DynamicSharedInformerFactory {
	return f.factories[ns]
}

// SetActiveNS sets the active namespace.
// BOZO!! Check ns access for resource??
func (f *Factory) SetActiveNS(ns string) {
	if !f.isClusterWide() {
		f.ensureFactory(ns)
	}
}

func (f *Factory) isClusterWide() bool {
	_, ok := f.factories[allNamespaces]
	return ok
}

// CanForResource return an informer is user has access.
func (f *Factory) CanForResource(ns, gvr string, verbs []string) (informers.GenericInformer, error) {
	// If user can access resource cluster wide, prefer cluster wide factory.
	if ns != allNamespaces {
		auth, err := f.Client().CanI(allNamespaces, gvr, verbs)
		if auth && err == nil {
			return f.ForResource(allNamespaces, gvr), nil
		}
	}
	auth, err := f.Client().CanI(ns, gvr, verbs)
	if err != nil {
		return nil, err
	}
	if !auth {
		return nil, fmt.Errorf("%v access denied on resource %q:%q", verbs, ns, gvr)
	}

	return f.ForResource(ns, gvr), nil
}

// ForResource returns an informer for a given resource.
func (f *Factory) ForResource(ns, gvr string) informers.GenericInformer {
	fact := f.ensureFactory(ns)
	inf := fact.ForResource(toGVR(gvr))
	if inf == nil {
		log.Error().Err(fmt.Errorf("MEOW! No informer for %q:%q", ns, gvr))
		return inf
	}
	log.Debug().Msgf("FOR_RESOURCE %q:%q", ns, gvr)
	fact.Start(f.stopChan)

	return inf
}

func (f *Factory) ensureFactory(ns string) di.DynamicSharedInformerFactory {
	if ns == clusterScope {
		ns = allNamespaces
	}
	if fac, ok := f.factories[ns]; ok {
		return fac
	}

	log.Debug().Msgf("FACTORY_NEW for ns %q", ns)
	f.factories[ns] = di.NewFilteredDynamicSharedInformerFactory(
		f.client.DynDialOrDie(),
		defaultResync,
		ns,
		nil,
	)

	return f.factories[ns]
}

// AddForwarder registers a new portforward for a given container.
func (f *Factory) AddForwarder(pf Forwarder) {
	f.forwarders[pf.Path()] = pf
}

// DeleteForwarder deletes portforward for a given container.
func (f *Factory) DeleteForwarder(path string) {

	f.forwarders.Dump()
	count := f.forwarders.Kill(path)
	log.Warn().Msgf("Deleted (%d) portforward for %q", count, path)
}

// Forwarders returns all portforwards.
func (f *Factory) Forwarders() Forwarders {
	return f.forwarders
}

// ForwarderFor returns a portforward for a given container or nil if none exists.
func (f *Factory) ForwarderFor(path string) (Forwarder, bool) {
	fwd, ok := f.forwarders[path]
	return fwd, ok
}

package factory

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	coreinformersv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/openshift/library-go/pkg/operator/events"
)

func makeFakeSecret() *v1.Secret {
	return &v1.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      "test-secret",
			Namespace: "test",
		},
		Data: map[string][]byte{
			"test": {},
		},
	}
}

type FakeController struct {
	synced chan struct{}
	t      *testing.T
}

func NewFakeController(t *testing.T, synced chan struct{}, secretsInformer coreinformersv1.SecretInformer) Controller {
	factory := New().WithInformers(secretsInformer.Informer())
	controller := &FakeController{synced: synced, t: t}
	return factory.WithSync(controller.Sync).WithRuntimeObject().ToController("FakeController", events.NewInMemoryRecorder("fake-controller"))
}

func (f *FakeController) Sync(ctx context.Context, controllerContext SyncContext) error {
	defer close(f.synced)
	if ctx.Err() != nil {
		f.t.Logf("syncContext %v", ctx.Err())
		return ctx.Err()
	}
	if name := controllerContext.GetObject().(*v1.Secret).GetName(); name != "test-secret" {
		f.t.Errorf("expected controller context to give secret name 'test-secret', got %q", name)
	}
	if _, ok := controllerContext.GetObject().(*v1.Secret); !ok {
		f.t.Errorf("expected Secret object, got %+v", controllerContext.GetObject())
	}
	f.t.Logf("controller sync called")
	return nil
}

func TestEmbeddedController(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 3*time.Second, informers.WithNamespace("test"))
	ctx, cancel := context.WithCancel(context.TODO())

	go kubeInformers.Start(ctx.Done())

	controllerSynced := make(chan struct{})
	controller := NewFakeController(t, controllerSynced, kubeInformers.Core().V1().Secrets())
	go controller.Run(ctx, 1)

	time.Sleep(1 * time.Second) // Give controller time to start
	if _, err := kubeClient.CoreV1().Secrets("test").Create(makeFakeSecret()); err != nil {
		t.Fatalf("failed to create fake secret: %v", err)
	}

	select {
	case <-controllerSynced:
		cancel()
	case <-time.After(30 * time.Second):
		t.Fatal("test timeout")
	}
}

func TestResyncController(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	factory := New().ResyncEvery(100 * time.Millisecond)

	controllerSynced := make(chan struct{})
	syncCallCount := 0
	controller := factory.WithSync(func(ctx context.Context, controllerContext SyncContext) error {
		syncCallCount++
		if syncCallCount == 3 {
			defer close(controllerSynced)
		}
		t.Logf("controller sync called (%d)", syncCallCount)
		return nil
	}).ToController("PeriodicController", events.NewInMemoryRecorder("periodic-controller"))

	go controller.Run(ctx, 1)
	time.Sleep(1 * time.Second) // Give controller time to start

	select {
	case <-controllerSynced:
		cancel()
	case <-time.After(10 * time.Second):
		t.Fatal("failed to resync at least three times")
	}
}

func TestMultiWorkerControllerShutdown(t *testing.T) {
	controllerCtx, shutdown := context.WithCancel(context.TODO())
	factory := New().ResyncEvery(5 * time.Minute) // make sure we only call 1 sync manually
	var workersShutdownMutex sync.Mutex
	workersShutdownCount := 0
	syncCallCount := 0
	var syncCallCountMutex sync.Mutex
	recorder := events.NewInMemoryRecorder("shutdown-controller")

	// simulate a long running sync logic that is signalled to shutdown
	controller := factory.WithSync(func(ctx context.Context, syncContext SyncContext) error {
		defer t.Logf("shutting down sync()")
		syncCallCountMutex.Lock()
		t.Logf("starting %d sync()", syncCallCount)
		syncCallCount++
		// First sync() enqueue 5 items which will result to all 5 workers having a job
		if syncCallCount == 1 {
			syncContext.Queue().Add("TestKey1")
			syncContext.Queue().Add("TestKey2")
			syncContext.Queue().Add("TestKey3")
			syncContext.Queue().Add("TestKey4")
			syncContext.Queue().Add("TestKey5")
		}
		syncCallCountMutex.Unlock()
		// This will make the worker stucked until shutdown is called
		<-ctx.Done()
		time.Sleep(1 * time.Second) // simulate worker being busy finishing
		workersShutdownMutex.Lock()
		workersShutdownCount++
		workersShutdownMutex.Unlock()
		return nil
	}).ToController("ShutdownController", recorder)

	runComplete := make(chan struct{})
	go func() {
		defer close(runComplete)
		controller.Run(controllerCtx, 5)
	}() // start controller with 5 workers

	time.Sleep(3 * time.Second) // give it time to periodically resync and call the sync() function

	shutdown() // initiate controller shutdown
	<-runComplete

	workersShutdownMutex.Lock()
	if workersShutdownCount != 6 {
		t.Fatalf("expected all 6 workers to gracefully shutdown, got %d", workersShutdownCount)
	}
	workersShutdownMutex.Unlock()
}

func TestSimpleController(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))
	ctx, cancel := context.WithCancel(context.TODO())

	go kubeInformers.Start(ctx.Done())
	factory := New().WithInformers(kubeInformers.Core().V1().Secrets().Informer())

	controllerSynced := make(chan struct{})
	controller := factory.WithSync(func(ctx context.Context, syncContext SyncContext) error {
		defer close(controllerSynced)
		t.Logf("controller sync called")
		if syncContext.GetObject() != nil {
			t.Errorf("expected queue object to be nil, it is %+v", syncContext.GetObject())
		}
		if syncContext.Queue() == nil {
			t.Errorf("expected queue to be initialized, it is not")
		}
		return nil
	}).ToController("FakeController", events.NewInMemoryRecorder("fake-controller"))

	go controller.Run(ctx, 1)
	time.Sleep(1 * time.Second) // Give controller time to start

	if _, err := kubeClient.CoreV1().Secrets("test").Create(makeFakeSecret()); err != nil {
		t.Fatalf("failed to create fake secret: %v", err)
	}

	select {
	case <-controllerSynced:
		cancel()
	case <-time.After(30 * time.Second):
		t.Fatal("test timeout")
	}
}

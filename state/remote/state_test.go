package remote

import (
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/states"
	"github.com/hashicorp/terraform/states/statemgr"
	"github.com/hashicorp/terraform/version"
)

func TestState_impl(t *testing.T) {
	var _ statemgr.Reader = new(State)
	var _ statemgr.Writer = new(State)
	var _ statemgr.Persister = new(State)
	var _ statemgr.Refresher = new(State)
	var _ statemgr.Locker = new(State)
}

func TestStateRace(t *testing.T) {
	s := &State{
		Client: nilClient{},
	}

	current := states.NewState()

	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.WriteState(current)
			s.PersistState()
			s.RefreshState()
		}()
	}
	wg.Wait()
}

func TestStatePersistNew(t *testing.T) {
	type testCase struct {
		name string
		// A function to mutate state and return a cleanup function
		mutationFunc func(*State) func()
		// The expected request to have taken place
		expectedRequest mockClientRequest
	}

	testCases := []testCase{
		// Refreshing state before we run the test loop causes a GET
		testCase{
			name: "refresh state",
			mutationFunc: func(mgr *State) func() {
				return func() {}
			},
			expectedRequest: mockClientRequest{
				Method: "Get",
				Content: map[string]interface{}{
					"version":           4.0, // encoding/json decodes this as float64 by default
					"lineage":           "mock-lineage",
					"serial":            1.0, // encoding/json decodes this as float64 by default
					"terraform_version": "0.0.0",
					"outputs":           map[string]interface{}{},
					"resources":         []interface{}{},
				},
			},
		},
		testCase{
			name: "change lineage",
			mutationFunc: func(mgr *State) func() {
				originalLineage := mgr.lineage
				mgr.lineage = "some-new-lineage"
				return func() {
					mgr.lineage = originalLineage
				}
			},
			expectedRequest: mockClientRequest{
				Method: "Put",
				Content: map[string]interface{}{
					"version":           4.0, // encoding/json decodes this as float64 by default
					"lineage":           "some-new-lineage",
					"serial":            2.0, // encoding/json decodes this as float64 by default
					"terraform_version": version.Version,
					"outputs":           map[string]interface{}{},
					"resources":         []interface{}{},
				},
			},
		},
		testCase{
			name: "change serial",
			mutationFunc: func(mgr *State) func() {
				originalSerial := mgr.serial
				mgr.serial++
				return func() {
					mgr.serial = originalSerial
				}
			},
			expectedRequest: mockClientRequest{
				Method: "Put",
				Content: map[string]interface{}{
					"version":           4.0, // encoding/json decodes this as float64 by default
					"lineage":           "mock-lineage",
					"serial":            4.0, // encoding/json decodes this as float64 by default
					"terraform_version": version.Version,
					"outputs":           map[string]interface{}{},
					"resources":         []interface{}{},
				},
			},
		},
		testCase{
			name: "change a state value",
			mutationFunc: func(mgr *State) func() {
				mgr.State().RootModule().SetOutputValue("foo", cty.StringVal("bar"), false)
				return func() {}
			},
			expectedRequest: mockClientRequest{
				Method: "Put",
				Content: map[string]interface{}{
					"version":           4.0, // encoding/json decodes this as float64 by default
					"lineage":           "mock-lineage",
					"serial":            3.0, // encoding/json decodes this as float64 by default
					"terraform_version": version.Version,
					"outputs": map[string]interface{}{
						"foo": map[string]interface{}{
							"type":  "string",
							"value": "baz",
						},
					},
					"resources": []interface{}{},
				},
			},
		},
	}

	// Initial setup of state just to give us a fixed starting point for our
	// test assertions below, or else we'd need to deal with
	// random lineage.
	mgr := &State{
		Client: &mockClient{
			current: []byte(`
				{
					"version": 4,
					"lineage": "mock-lineage",
					"serial": 1,
					"terraform_version":"0.0.0",
					"outputs": {},
					"resources": []
				}
			`),
		},
	}

	// In normal use (during a Terraform operation) we always refresh and read
	// before any writes would happen, so we'll mimic that here for realism.
	// NB This causes a GET to be logged so the first item in the test cases
	// must account for this
	if err := mgr.RefreshState(); err != nil {
		t.Fatalf("failed to RefreshState: %s", err)
	}

	// Run tests in order.
	for i, tc := range testCases {
		cleanup := tc.mutationFunc(mgr)

		s := mgr.State()
		if err := mgr.WriteState(s); err != nil {
			t.Fatalf("failed to WriteState for %s: %s", tc.name, err)
		}
		if err := mgr.PersistState(); err != nil {
			t.Fatalf("failed to PersistState for %s: %s", tc.name, err)
		}

		// Get captured request from the mock client log
		// based on the index of the current test
		loggedRequest := mgr.Client.(*mockClient).log[i]
		if diff := cmp.Diff(tc.expectedRequest, loggedRequest); len(diff) > 0 {
			t.Fatalf("incorrect client requests for %s:\n%s", tc.name, diff)
		}
		cleanup()
	}
}

func TestStatePersist(t *testing.T) {
	mgr := &State{
		Client: &mockClient{
			// Initial state just to give us a fixed starting point for our
			// test assertions below, or else we'd need to deal with
			// random lineage.
			current: []byte(`
				{
					"version": 4,
					"lineage": "mock-lineage",
					"serial": 1,
					"terraform_version":"0.0.0",
					"outputs": {},
					"resources": []
				}
			`),
		},
	}

	// In normal use (during a Terraform operation) we always refresh and read
	// before any writes would happen, so we'll mimic that here for realism.
	if err := mgr.RefreshState(); err != nil {
		t.Fatalf("failed to RefreshState: %s", err)
	}
	s := mgr.State()

	s.RootModule().SetOutputValue("foo", cty.StringVal("bar"), false)
	if err := mgr.WriteState(s); err != nil {
		t.Fatalf("failed to WriteState: %s", err)
	}
	if err := mgr.PersistState(); err != nil {
		t.Fatalf("failed to PersistState: %s", err)
	}

	// Persisting the same state again should be a no-op: it doesn't fail,
	// but it ought not to appear in the client's log either.
	if err := mgr.WriteState(s); err != nil {
		t.Fatalf("failed to WriteState: %s", err)
	}
	if err := mgr.PersistState(); err != nil {
		t.Fatalf("failed to PersistState: %s", err)
	}

	// We also don't persist state if the lineage or the serial change
	originalSerial := mgr.serial
	mgr.serial++
	if err := mgr.WriteState(s); err != nil {
		t.Fatalf("failed to WriteState: %s", err)
	}
	if err := mgr.PersistState(); err != nil {
		t.Fatalf("failed to PersistState: %s", err)
	}
	mgr.serial = originalSerial

	originalLineage := mgr.lineage
	mgr.lineage = "behold-a-wild-lineage-appears"
	if err := mgr.WriteState(s); err != nil {
		t.Fatalf("failed to WriteState: %s", err)
	}
	if err := mgr.PersistState(); err != nil {
		t.Fatalf("failed to PersistState: %s", err)
	}
	mgr.lineage = originalLineage

	// ...but if we _do_ change something in the state then we should see
	// it re-persist.
	s.RootModule().SetOutputValue("foo", cty.StringVal("baz"), false)
	if err := mgr.WriteState(s); err != nil {
		t.Fatalf("failed to WriteState: %s", err)
	}
	if err := mgr.PersistState(); err != nil {
		t.Fatalf("failed to PersistState: %s", err)
	}

	got := mgr.Client.(*mockClient).log
	want := []mockClientRequest{
		// The initial fetch from mgr.RefreshState above.
		{
			Method: "Get",
			Content: map[string]interface{}{
				"version":           4.0, // encoding/json decodes this as float64 by default
				"lineage":           "mock-lineage",
				"serial":            1.0, // encoding/json decodes this as float64 by default
				"terraform_version": "0.0.0",
				"outputs":           map[string]interface{}{},
				"resources":         []interface{}{},
			},
		},

		// First call to PersistState, with output "foo" set to "bar".
		{
			Method: "Put",
			Content: map[string]interface{}{
				"version":           4.0,
				"lineage":           "mock-lineage",
				"serial":            2.0, // serial increases because the outputs changed
				"terraform_version": version.Version,
				"outputs": map[string]interface{}{
					"foo": map[string]interface{}{
						"type":  "string",
						"value": "bar",
					},
				},
				"resources": []interface{}{},
			},
		},

		// Second call to PersistState generates no client requests, because
		// nothing changed in the state itself.

		// Third call to PersistState, with the serial changed
		{
			Method: "Put",
			Content: map[string]interface{}{
				"version":           4.0,
				"lineage":           "mock-lineage",
				"serial":            4.0, // serial increases because the outputs changed
				"terraform_version": version.Version,
				"outputs": map[string]interface{}{
					"foo": map[string]interface{}{
						"type":  "string",
						"value": "bar",
					},
				},
				"resources": []interface{}{},
			},
		},

		// Fourth call to PersistState, with the lineage changed
		{
			Method: "Put",
			Content: map[string]interface{}{
				"version":           4.0,
				"lineage":           "behold-a-wild-lineage-appears",
				"serial":            3.0, // serial increases because the outputs changed
				"terraform_version": version.Version,
				"outputs": map[string]interface{}{
					"foo": map[string]interface{}{
						"type":  "string",
						"value": "bar",
					},
				},
				"resources": []interface{}{},
			},
		},

		// Fifth call to PersistState, with the "foo" output value updated
		// to "baz".
		{
			Method: "Put",
			Content: map[string]interface{}{
				"version":           4.0,
				"lineage":           "mock-lineage",
				"serial":            4.0, // serial increases because the outputs changed
				"terraform_version": version.Version,
				"outputs": map[string]interface{}{
					"foo": map[string]interface{}{
						"type":  "string",
						"value": "baz",
					},
				},
				"resources": []interface{}{},
			},
		},
	}
	if diff := cmp.Diff(want, got); len(diff) > 0 {
		t.Errorf("incorrect client requests\n%s", diff)
	}
}

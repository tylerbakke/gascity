package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// identityBeadStore is a minimal beads.Store stub used only to compare store
// identity in these tests. It embeds a nil beads.Store, so any method call
// other than identity comparison panics — the tests must never invoke one.
type identityBeadStore struct {
	beads.Store
	id string
}

// TestRigBeadStoresExcludesCityStore verifies the baseline contract:
// rigBeadStores() returns only rig stores and never the city's own store.
func TestRigBeadStoresExcludesCityStore(t *testing.T) {
	cityStore := &identityBeadStore{id: "city"}
	rigStore := &identityBeadStore{id: "rig"}
	cs := &controllerState{
		cityName:      "test-city",
		cityBeadStore: cityStore,
		beadStores:    map[string]beads.Store{"backend": rigStore},
	}
	cr := &CityRuntime{cityName: "test-city", cs: cs}

	got := cr.rigBeadStores()
	if len(got) != 1 {
		t.Fatalf("rigBeadStores() len = %d, want 1 (rig only)", len(got))
	}
	if got["backend"] != beads.Store(rigStore) {
		t.Errorf("rigBeadStores()[backend] = %v, want the rig store", got["backend"])
	}
	if _, ok := got["test-city"]; ok {
		t.Error("rigBeadStores() must not include the city store under the city name key")
	}
}

// TestRigBeadStoresKeepsRigNamedLikeCity is the regression test for the bug:
// when a rig's name equals the city name, the rig store must survive. The
// city's own store lives in a separate field, so excluding it must not evict
// a legitimately same-named rig store.
func TestRigBeadStoresKeepsRigNamedLikeCity(t *testing.T) {
	cityStore := &identityBeadStore{id: "city"}
	collidingRigStore := &identityBeadStore{id: "rig-colliding"}
	otherRigStore := &identityBeadStore{id: "rig-other"}
	cs := &controllerState{
		cityName:      "command-center",
		cityBeadStore: cityStore,
		beadStores: map[string]beads.Store{
			"command-center": collidingRigStore, // rig named identically to the city
			"backend":        otherRigStore,
		},
	}
	cr := &CityRuntime{cityName: "command-center", cs: cs}

	got := cr.rigBeadStores()
	if len(got) != 2 {
		t.Fatalf("rigBeadStores() len = %d, want 2 (both rigs survive)", len(got))
	}
	if got["command-center"] != beads.Store(collidingRigStore) {
		t.Errorf("rigBeadStores()[command-center] = %v, want the rig store (not the city store)", got["command-center"])
	}
	if got["command-center"] == beads.Store(cityStore) {
		t.Error("rigBeadStores()[command-center] must be the rig store, never the city store")
	}
	if got["backend"] != beads.Store(otherRigStore) {
		t.Errorf("rigBeadStores()[backend] = %v, want the other rig store", got["backend"])
	}
}

// TestRigBeadStoresReturnsCopy verifies the caller receives an independent map:
// mutating the result must not corrupt the controller's live rig store map.
func TestRigBeadStoresReturnsCopy(t *testing.T) {
	rigStore := &identityBeadStore{id: "rig"}
	cs := &controllerState{
		cityName:      "test-city",
		cityBeadStore: &identityBeadStore{id: "city"},
		beadStores:    map[string]beads.Store{"backend": rigStore},
	}
	cr := &CityRuntime{cityName: "test-city", cs: cs}

	got := cr.rigBeadStores()
	delete(got, "backend")

	if cs.BeadStore("backend") != beads.Store(rigStore) {
		t.Error("mutating rigBeadStores() result corrupted the controller's live rig store map")
	}
}

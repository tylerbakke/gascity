package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestManagedDoltLifecycleLockPathForCityMatchesCanonicalLayout(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	got := managedDoltLifecycleLockPathForCity(cityPath)
	want := filepath.Join(citylayout.PackStateDir(cityPath, "dolt"), "dolt.lock")
	if got != want {
		t.Fatalf("managedDoltLifecycleLockPathForCity(%q) = %q, want %q", cityPath, got, want)
	}
}

func TestManagedDoltLifecycleLockPathForCityRejectsEmpty(t *testing.T) {
	if got := managedDoltLifecycleLockPathForCity(""); got != "" {
		t.Fatalf("managedDoltLifecycleLockPathForCity(\"\") = %q, want \"\"", got)
	}
	if got := managedDoltLifecycleLockPathForCity("   "); got != "" {
		t.Fatalf("managedDoltLifecycleLockPathForCity(\"   \") = %q, want \"\"", got)
	}
}

func TestRemoveStaleManagedDoltLifecycleLockSkipsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dolt.lock")
	if err := removeStaleManagedDoltLifecycleLock(path); err != nil {
		t.Fatalf("removeStaleManagedDoltLifecycleLock(missing) error = %v, want nil", err)
	}
}

func TestRemoveStaleManagedDoltLifecycleLockSkipsEmptyPath(t *testing.T) {
	if err := removeStaleManagedDoltLifecycleLock(""); err != nil {
		t.Fatalf("removeStaleManagedDoltLifecycleLock(\"\") error = %v, want nil", err)
	}
	if err := removeStaleManagedDoltLifecycleLock("   "); err != nil {
		t.Fatalf("removeStaleManagedDoltLifecycleLock(blank) error = %v, want nil", err)
	}
}

func TestRemoveStaleManagedDoltLifecycleLockSkipsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dolt.lock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleManagedDoltLifecycleLock(dir); err != nil {
		t.Fatalf("removeStaleManagedDoltLifecycleLock(dir) error = %v, want nil", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory was removed: %v", err)
	}
}

func TestRemoveStaleManagedDoltLifecycleLockRemovesWhenStateKnownAndClosed(t *testing.T) {
	skipSlowCmdGCTest(t, "exercises managed dolt lock self-heal against /proc or lsof; run make test-cmd-gc-process for full coverage")
	path := filepath.Join(t.TempDir(), "dolt.lock")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(t.TempDir(), "missing-bin"))

	_, procChecked := fileOpenedByAnyProcessFromProc(path)
	if err := removeStaleManagedDoltLifecycleLock(path); err != nil {
		t.Fatalf("removeStaleManagedDoltLifecycleLock: %v", err)
	}
	if !procChecked {
		// open-state unknown: file must be preserved.
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat after cleanup = %v, want file preserved when state unknown", err)
		}
		return
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stat after cleanup = %v, want stale lock removed", err)
	}
}

func TestRemoveStaleManagedDoltLifecycleLockPreservesOpenFile(t *testing.T) {
	skipSlowCmdGCTest(t, "holds the dolt lock open and verifies the cleanup leaves it alone; run make test-cmd-gc-process for full coverage")
	path := filepath.Join(t.TempDir(), "dolt.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	_, procChecked := fileOpenedByAnyProcessFromProc(path)
	if !procChecked {
		// On systems where /proc isn't readable and lsof is missing,
		// open-state is unknown — the cleanup preserves regardless. The
		// "preserve open file" assertion needs a working signal.
		t.Skip("/proc fd scan unavailable; cannot validate open-file preservation deterministically")
	}
	if err := removeStaleManagedDoltLifecycleLock(path); err != nil {
		t.Fatalf("removeStaleManagedDoltLifecycleLock: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat after cleanup = %v, want open lock preserved", err)
	}
}

func TestCleanupStaleManagedDoltLifecycleLocksForSupervisorStartRequiresGCHome(t *testing.T) {
	if err := cleanupStaleManagedDoltLifecycleLocksForSupervisorStart(""); err == nil {
		t.Fatal("cleanupStaleManagedDoltLifecycleLocksForSupervisorStart(\"\") error = nil, want error")
	}
	if err := cleanupStaleManagedDoltLifecycleLocksForSupervisorStart("   "); err == nil {
		t.Fatal("cleanupStaleManagedDoltLifecycleLocksForSupervisorStart(blank) error = nil, want error")
	}
}

func TestCleanupStaleManagedDoltLifecycleLocksForSupervisorStartSkipsMissingPackDir(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("Register(%q): %v", cityPath, err)
	}

	if err := cleanupStaleManagedDoltLifecycleLocksForSupervisorStart(gcHome); err != nil {
		t.Fatalf("cleanupStaleManagedDoltLifecycleLocksForSupervisorStart: %v", err)
	}
}

func TestCleanupStaleManagedDoltLifecycleLocksForSupervisorStartRemovesStaleLock(t *testing.T) {
	skipSlowCmdGCTest(t, "iterates registered cities and removes stale dolt.lock files; run make test-cmd-gc-process for full coverage")
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("PATH", filepath.Join(t.TempDir(), "missing-bin"))

	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := managedDoltLifecycleLockPathForCity(cityPath)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("Register(%q): %v", cityPath, err)
	}

	_, procChecked := fileOpenedByAnyProcessFromProc(lockPath)
	if err := cleanupStaleManagedDoltLifecycleLocksForSupervisorStart(gcHome); err != nil {
		t.Fatalf("cleanupStaleManagedDoltLifecycleLocksForSupervisorStart: %v", err)
	}
	if !procChecked {
		// State unknown: cleanup preserves on the belt-and-suspenders principle.
		if _, err := os.Stat(lockPath); err != nil {
			t.Fatalf("stat lock after cleanup = %v, want preserved when state unknown", err)
		}
		return
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stat lock after cleanup = %v, want removed", err)
	}
}

func TestCleanupStaleManagedDoltLifecycleLocksForSupervisorStartDedupesCities(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("Register first: %v", err)
	}

	cities, err := managedDoltLifecycleLockCleanupCities(gcHome)
	if err != nil {
		t.Fatalf("managedDoltLifecycleLockCleanupCities: %v", err)
	}
	want := normalizePathForCompare(cityPath)
	if len(cities) != 1 || cities[0] != want {
		t.Fatalf("cities = %+v, want [%q]", cities, want)
	}
}

func TestManagedDoltLifecycleLockCleanupCitiesPropagatesRegistryError(t *testing.T) {
	// Force RegistryPath to point at a path the registry cannot read.
	homeDir := t.TempDir()
	gcHome := filepath.Join(homeDir, "gc-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)

	// Drop a non-file at the registry path so the registry's read fails
	// with a real I/O error, distinguishable from "no entries yet".
	regPath := supervisor.RegistryPath()
	if err := os.MkdirAll(regPath, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := managedDoltLifecycleLockCleanupCities(gcHome)
	if err == nil {
		t.Fatal("managedDoltLifecycleLockCleanupCities(corrupted registry) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "supervisor registry") {
		t.Fatalf("error %q does not mention supervisor registry", err)
	}
	if errors.Is(err, errManagedDoltOpenStateUnknown) {
		t.Fatalf("unexpected sentinel mixed into registry error: %v", err)
	}
}

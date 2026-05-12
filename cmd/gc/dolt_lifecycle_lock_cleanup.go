package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// cleanupStaleManagedDoltLifecycleLocksForSupervisorStart removes stale
// .gc/runtime/packs/dolt/dolt.lock flock files for every registered city
// at supervisor start. A lock file is "stale" when it exists on disk but
// no live process holds an open file descriptor for it.
//
// Background: the dolt.lock file is an advisory flock target used by
// `recoverManagedDoltProcess` to serialize lifecycle operations. The
// kernel releases the flock automatically when the holding process
// dies, so on paper a leftover file is harmless. In practice (incident
// 2026-05-11), operators recovering from a wedged supervisor / dolt
// cascade had to run `rm` on these files manually before
// `gc supervisor start` would proceed cleanly. This helper folds that
// step into supervisor startup so operators do not have to remember it.
//
// Matches the self-heal pattern that gp-yi2dk applied to stale git
// index.lock files: detect a leftover file with no owning process, then
// remove it before downstream code re-creates it.
//
// Best-effort: errors from individual cities are joined and returned to
// the caller so the supervisor can log them, but no error from this
// helper is treated as fatal. Cities whose pack state directory does
// not yet exist are skipped silently.
func cleanupStaleManagedDoltLifecycleLocksForSupervisorStart(gcHome string) error {
	cityPaths, err := managedDoltLifecycleLockCleanupCities(gcHome)
	if err != nil {
		return err
	}
	var errs []error
	for _, cityPath := range cityPaths {
		lockPath := managedDoltLifecycleLockPathForCity(cityPath)
		if lockPath == "" {
			continue
		}
		if err := removeStaleManagedDoltLifecycleLock(lockPath); err != nil {
			errs = append(errs, fmt.Errorf("city %q: %w", cityPath, err))
		}
	}
	return errors.Join(errs...)
}

// managedDoltLifecycleLockCleanupCities returns the deduped, ordered set
// of registered city paths whose dolt.lock files should be considered
// for cleanup. Reading the supervisor registry directly (rather than
// the workspace-service scope helper) keeps the cleanup independent
// from workspace-service ownership rules.
func managedDoltLifecycleLockCleanupCities(gcHome string) ([]string, error) {
	if strings.TrimSpace(gcHome) == "" {
		return nil, errors.New("missing GC_HOME for managed dolt lifecycle lock cleanup")
	}
	entries, err := supervisor.NewRegistry(supervisor.RegistryPath()).List()
	if err != nil {
		return nil, fmt.Errorf("reading supervisor registry for managed dolt lifecycle lock cleanup: %w", err)
	}
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		cityPath := normalizePathForCompare(strings.TrimSpace(entry.Path))
		if cityPath == "" {
			continue
		}
		if _, ok := seen[cityPath]; ok {
			continue
		}
		seen[cityPath] = struct{}{}
		out = append(out, cityPath)
	}
	return out, nil
}

// managedDoltLifecycleLockPathForCity returns the canonical absolute
// path of the managed dolt.lock file for the given city. Returns ""
// when the city path is blank. Does not consult environment variables
// — supervisor startup runs in a single process whose env is shared
// across all cities, so the canonical layout is the only safe choice.
func managedDoltLifecycleLockPathForCity(cityPath string) string {
	cityPath = strings.TrimSpace(cityPath)
	if cityPath == "" {
		return ""
	}
	return filepath.Join(citylayout.PackStateDir(cityPath, "dolt"), "dolt.lock")
}

// removeStaleManagedDoltLifecycleLock removes lockPath when it exists
// on disk but no live process has it open. Returns nil when the file
// is missing, currently in use, or its open-state cannot be
// determined (in which case the lock is preserved on the
// belt-and-suspenders principle).
func removeStaleManagedDoltLifecycleLock(lockPath string) error {
	lockPath = strings.TrimSpace(lockPath)
	if lockPath == "" {
		return nil
	}
	info, err := os.Lstat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return nil
	}
	open, err := fileOpenedByAnyProcess(lockPath)
	if err != nil {
		if errors.Is(err, errManagedDoltOpenStateUnknown) {
			return nil
		}
		return err
	}
	if open {
		return nil
	}
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

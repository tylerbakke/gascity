package builtinpacks_test

import (
	"io/fs"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/orders"
)

// TestBundledGastownDigestGenerateOrderIsCityScoped guards the city-scoping of
// the bundled gastown `digest-generate` order against silent regression.
//
// digest-generate pours one daily digest that already covers every rig, so it
// must register exactly once at the city level. When it is left rig-scoped
// (the default), the gastown pack's per-rig import projects a separate
// digest-generate instance onto every importing rig, each with its own
// `order-run:digest-generate:rig:<rig>` cooldown anchor. Those independent
// clocks fan the 24h-interval order out into one dispatch per rig per period —
// the duplicate-digest / duplicate-mayor-mail spawn storm (command-center
// ci-r0zhzc): N "Digest: <date>" archive beads and N mayor mails per cycle.
//
// scope = "city" was added in gascity#3320 to collapse those per-rig copies to
// a single town-wide instance via orderdiscovery.ScanAll's cityScopedSeen
// dedup. That fix shipped with NO test referencing the real order (the order
// suites use synthetic fixtures), and the order then moved out of tree: the
// vendored copy was deleted in gascity#3335 and the gastown pack is now
// consumed from the pinned gascity-packs Go module embedded via
// builtinpacks.ByName("gastown"). Every pin bump (scripts/update-bundled-
// gastown-pack) re-imports that order's TOML, so a regressed upstream release
// could silently restore rig-scoping and re-arm the storm with nothing to
// catch it. This test asserts the property at the embed boundary; the per-rig
// dedup itself is covered by
// orderdiscovery.TestScanAllCityScopedOrderRegistersOnceAcrossRigs, so the two
// together guard the full "one town-wide run per period, independent of rig
// fan-out" chain.
func TestBundledGastownDigestGenerateOrderIsCityScoped(t *testing.T) {
	pack, ok := builtinpacks.ByName("gastown")
	if !ok {
		t.Fatal("gastown pack is not bundled")
	}

	const orderPath = "orders/digest-generate.toml"
	data, err := fs.ReadFile(pack.FS, orderPath)
	if err != nil {
		t.Fatalf("reading bundled gastown %s: %v", orderPath, err)
	}

	order, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("parsing bundled gastown %s: %v", orderPath, err)
	}
	// Name is derived from the discovered filename, not the TOML, so stamp it
	// the way the scanner would before validating.
	order.Name = "digest-generate"

	if err := orders.Validate(order); err != nil {
		t.Fatalf("bundled gastown digest-generate order is invalid: %v", err)
	}

	if !order.IsCityScoped() {
		t.Errorf("bundled gastown digest-generate order scope = %q, want \"city\": "+
			"a rig-scoped digest fans out one dispatch per importing rig per period "+
			"(duplicate-digest spawn storm, ci-r0zhzc); restore scope = \"city\" "+
			"(gascity#3320) in the gascity-packs release before bumping the pin", order.Scope)
	}

	// The storm only manifests for periodic formula dispatch: a cooldown trigger
	// pouring a formula. If either changes, the city-scoping rationale and this
	// guard's framing no longer hold, so pin the surrounding contract too.
	if order.Trigger != "cooldown" {
		t.Errorf("bundled gastown digest-generate trigger = %q, want \"cooldown\"", order.Trigger)
	}
	if order.Formula != "mol-digest-generate" {
		t.Errorf("bundled gastown digest-generate formula = %q, want \"mol-digest-generate\"", order.Formula)
	}
}

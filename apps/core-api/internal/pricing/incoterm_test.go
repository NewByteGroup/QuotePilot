package pricing

import "testing"

// Task 2 (spec §3): the import Incoterm matrix decides which cost
// groups the requesting customer owes. EXPORT, LCL and sea-incoterms on
// air exit early with reasons; DDP completes with zero lines.

func requireGroups(t *testing.T, in Input, want groups) {
	t.Helper()
	got, early := requiredGroups(in)
	if early != nil {
		t.Fatalf("unexpected early exit %+v", early)
	}
	if *got != want {
		t.Fatalf("incoterm %s: groups %+v, want %+v", in.Incoterm, *got, want)
	}
}

func TestRequiredGroups_EXWRequiresEverything(t *testing.T) {
	in := fobFCLInput()
	in.Incoterm = IncotermEXW
	requireGroups(t, in, groups{origin: true, freight: true, terminal: true, customs: true})
}

func TestRequiredGroups_FCARequiresOriginTerminalFees(t *testing.T) {
	in := fobFCLInput()
	in.Incoterm = IncotermFCA
	requireGroups(t, in, groups{origin: true, freight: true, terminal: true, customs: true})
}

func TestRequiredGroups_FOBAndFASDropOrigin(t *testing.T) {
	for _, inc := range []Incoterm{IncotermFOB, IncotermFAS} {
		in := fobFCLInput()
		in.Incoterm = inc
		requireGroups(t, in, groups{freight: true, terminal: true, customs: true})
	}
}

func TestRequiredGroups_CIFFFamilyDropsFreightAndOrigin(t *testing.T) {
	for _, inc := range []Incoterm{IncotermCFR, IncotermCIF, IncotermCPT, IncotermCIP} {
		in := fobFCLInput()
		in.Incoterm = inc
		requireGroups(t, in, groups{terminal: true, customs: true})
	}
}

func TestRequiredGroups_DAPDPUTerminalOnlyWhenRequested(t *testing.T) {
	for _, inc := range []Incoterm{IncotermDAP, IncotermDPU} {
		in := fobFCLInput()
		in.Incoterm = inc
		in.TerminalRequested = false
		requireGroups(t, in, groups{customs: true})

		in.TerminalRequested = true
		requireGroups(t, in, groups{terminal: true, customs: true})
	}
}

func TestRequiredGroups_DDPSkipsEverything(t *testing.T) {
	in := fobFCLInput()
	in.Incoterm = IncotermDDP
	g, early := requiredGroups(in)
	if early == nil || !early.ddp {
		t.Fatalf("DDP must exit early as nothing payable: %+v", early)
	}
	if g != nil {
		t.Fatal("DDP must not return a group set")
	}
}

func TestRequiredGroups_ExportIsIncomplete(t *testing.T) {
	in := fobFCLInput()
	in.Direction = DirectionExport
	_, early := requiredGroups(in)
	if early == nil || early.reason != ReasonExportMatrixOpen {
		t.Fatalf("EXPORT must exit with EXPORT_MATRIX_OPEN, got %+v", early)
	}
}

func TestRequiredGroups_LCLIsOutOfMVP(t *testing.T) {
	in := fobFCLInput()
	in.Modal = ModalLCL
	_, early := requiredGroups(in)
	if early == nil || early.reason != ReasonModalOutOfMVP {
		t.Fatalf("LCL must exit with MODAL_OUT_OF_MVP, got %+v", early)
	}
}

func TestRequiredGroups_SeaIncotermsRejectedOnAir(t *testing.T) {
	for _, inc := range []Incoterm{IncotermFAS, IncotermFOB, IncotermCFR, IncotermCIF} {
		in := fobFCLInput()
		in.Modal = ModalAIR
		in.ContainerCount = 0
		in.Incoterm = inc
		_, early := requiredGroups(in)
		if early == nil || early.reason != ReasonSeaIncotermOnAir {
			t.Fatalf("%s on AIR must exit with SEA_INCOTERM_ON_AIR, got %+v", inc, early)
		}
	}
}

func TestRequiredGroups_AirOnlyIncotermsAllowedOnAir(t *testing.T) {
	for _, inc := range []Incoterm{IncotermEXW, IncotermFCA, IncotermCPT, IncotermCIP, IncotermDAP, IncotermDPU} {
		in := fobFCLInput()
		in.Modal = ModalAIR
		in.ContainerCount = 0
		in.Incoterm = inc
		g, early := requiredGroups(in)
		if early != nil {
			t.Fatalf("%s on AIR must be allowed, got early %+v", inc, early)
		}
		if g == nil {
			t.Fatalf("%s on AIR must return groups", inc)
		}
	}
}

// Carrier surcharges are the freight payer's charges, not a required
// group: the pool opens only while freight is owed, and an empty pool
// never makes the quote INCOMPLETE.
func TestSurchargePool_FollowsFreightPayer(t *testing.T) {
	in := fobTariffInput("950.0000") // FOB: freight owed -> surcharge line.
	out := mustComplete(t, in)
	if findLine(out.Lines, CategorySurcharge, "BAF") == nil {
		t.Fatal("surcharge rows must emit lines while freight is owed")
	}

	in = fobTariffInput("950.0000")
	in.Incoterm = IncotermCIF // seller pays freight -> pool stays closed.
	in.DeclaredFreight = &Money{Amount: d("1500.00"), Currency: "USD"}
	out = mustComplete(t, in)
	if findLine(out.Lines, CategorySurcharge, "BAF") != nil {
		t.Fatal("CIF freight is the seller's: no surcharge line allowed")
	}
}

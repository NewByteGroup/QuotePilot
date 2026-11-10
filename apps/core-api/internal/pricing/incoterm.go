package pricing

// groups is the set of cost groups the requesting customer owes under
// the Incoterm (spec §3 import matrix). Carrier surcharges are not a
// matrix column and never a required group: they are billed to whoever
// pays the freight, so their pool is only collected while freight is
// owed — an empty surcharge pool is never a gap (spec §4 step 5).
type groups struct {
	origin   bool
	freight  bool
	terminal bool
	customs  bool
}

// earlyExit is a non-priced outcome decided before line building:
// either an INCOMPLETE reason or the DDP nothing-payable completion.
type earlyExit struct {
	reason Reason
	ddp    bool
}

// importMatrix is spec §3's import proposal (RFC-001 §4). DAP/DPU's
// terminal cell is conditional on TerminalRequested and handled in
// requiredGroups; DDP exits before the table applies.
var importMatrix = map[Incoterm]groups{
	IncotermEXW: {origin: true, freight: true, terminal: true, customs: true},
	IncotermFCA: {origin: true, freight: true, terminal: true, customs: true},
	IncotermFAS: {freight: true, terminal: true, customs: true},
	IncotermFOB: {freight: true, terminal: true, customs: true},
	IncotermCFR: {terminal: true, customs: true},
	IncotermCIF: {terminal: true, customs: true},
	IncotermCPT: {terminal: true, customs: true},
	IncotermCIP: {terminal: true, customs: true},
	IncotermDAP: {customs: true},
	IncotermDPU: {customs: true},
}

// requiredGroups resolves spec §3: EXPORT and non-MVP modals exit
// immediately, sea-only Incoterms on air are double-checked (inputs are
// untrusted), DDP completes with zero lines, and the import matrix
// decides the rest.
func requiredGroups(in Input) (*groups, *earlyExit) {
	if in.Direction == DirectionExport {
		return nil, &earlyExit{reason: ReasonExportMatrixOpen}
	}
	if in.Modal != ModalFCL && in.Modal != ModalAIR {
		return nil, &earlyExit{reason: ReasonModalOutOfMVP}
	}
	if in.Modal == ModalAIR && seaOnly(in.Incoterm) {
		return nil, &earlyExit{reason: ReasonSeaIncotermOnAir}
	}
	if in.Incoterm == IncotermDDP {
		return nil, &earlyExit{ddp: true}
	}
	g := importMatrix[in.Incoterm]
	if (in.Incoterm == IncotermDAP || in.Incoterm == IncotermDPU) && in.TerminalRequested {
		g.terminal = true
	}
	return &g, nil
}

// seaOnly reports sea-and-inland-waterway-only Incoterms 2020, which the
// extractor flags upstream and the engine double-checks (spec §3).
func seaOnly(i Incoterm) bool {
	switch i {
	case IncotermFAS, IncotermFOB, IncotermCFR, IncotermCIF:
		return true
	default:
		return false
	}
}

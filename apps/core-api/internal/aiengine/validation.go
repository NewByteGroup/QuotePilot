package aiengine

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
)

// Extraction contract validation (AGENTS.md rule 6 + spec S2): every
// ai-engine response is untrusted — core checks schema shape and sanity
// bounds before persisting anything to extracted_payload_json. The checks
// here are the wire-level subset of RFC-004 section 4: required keys,
// contract version, probability bounds and per-field envelopes.

// contractVersion is the single ExtractionResult contract version core
// accepts today (packages/contracts/schemas/extraction_result.schema.json).
const contractVersion = "1.0.0"

// referenceDatasetVersion is the bundled reference dataset (UN/LOCODE, NCM,
// Incoterms) the extractor must have normalized against. Provisional pin —
// core does not ship the dataset artifact yet; when it lands this constant
// is updated to its version (RFC-004 section 4).
const referenceDatasetVersion = "2026-11-10"

// probSumEpsilon is how far classification probabilities may drift from 1
// before being rejected (calibrated softmax output, fp tolerance).
const probSumEpsilon = 0.02

var semverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// evidenceSourcePattern is common.schema.json's EvidenceSource:
// EMAIL_BODY or a named attachment — the web highlight split depends on it.
var evidenceSourcePattern = regexp.MustCompile(`^(EMAIL_BODY|ATTACHMENT:.+)$`)

// knownFields is the closed key set of the contract's `fields` object
// (additionalProperties: false in the schema — an unknown key is a
// violation, not a free-form extension point).
var knownFields = map[string]bool{
	"incoterm": true, "modal": true,
	"origin_locode": true, "origin_text": true,
	"dest_locode": true, "dest_text": true,
	"equipment_or_unit": true, "quantity": true,
	"gross_weight_kg": true, "volume_m3": true,
	"cargo_value_amount": true, "cargo_value_currency": true,
	"declared_freight_amount": true, "declared_freight_currency": true,
	"declared_insurance_amount": true, "declared_insurance_currency": true,
	"ncm": true, "ready_date": true,
}

// ExtractionReport is the validated slice the worker needs: the identity
// recorded for audit plus the gate input.
type ExtractionReport struct {
	ContractVersion string
	// MinConfidence is the minimum auditor confidence across all field
	// envelopes — what the column documents (RFC-004 section 5). Zero
	// fields means zero confidence.
	MinConfidence float64
	FieldCount    int
}

// extractionDoc is the decode shape for the required keys only — per-field
// envelopes are checked separately because they carry the gate input.
// Fields is a pointer so a missing/null `fields` key fails as required
// rather than decoding like a valid empty object.
type extractionDoc struct {
	ContractVersion string                    `json:"contract_version"`
	Classification  *extractionClassification `json:"classification"`
	Fields          *map[string]fieldEnvelope `json:"fields"`
	DatasetVersion  string                    `json:"reference_dataset_version"`
	ModelInfo       *extractionModelInfo      `json:"model_info"`
}

type extractionClassification struct {
	Label         string             `json:"label"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type extractionModelInfo struct {
	ExtractorModel string `json:"extractor_model"`
	AuditorModel   string `json:"auditor_model"`
}

// fieldEnvelope is the common.schema.json FieldValue shape at the level
// core validates (value/evidence/inferred/confidence/reasons required).
type fieldEnvelope struct {
	Value    *string `json:"value"`
	Evidence *struct {
		Source     string `json:"source"`
		ExactQuote string `json:"exact_quote"`
	} `json:"evidence"`
	Inferred   *bool     `json:"inferred"`
	Confidence *float64  `json:"confidence"`
	Reasons    *[]string `json:"reasons"`
}

// ValidateExtraction decodes and checks one ExtractionResult blob. A nil
// error means the payload is safe to persist verbatim.
func ValidateExtraction(raw json.RawMessage) (*ExtractionReport, error) {
	var doc extractionDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode extraction: %w", err)
	}
	if err := checkRequired(&doc); err != nil {
		return nil, err
	}
	if err := checkClassification(doc.Classification); err != nil {
		return nil, err
	}
	return checkFields(*doc.Fields)
}

// checkRequired verifies identity keys: version pins and model info.
func checkRequired(doc *extractionDoc) error {
	if !semverPattern.MatchString(doc.ContractVersion) {
		return fmt.Errorf("contract_version %q is not semver", doc.ContractVersion)
	}
	if doc.ContractVersion != contractVersion {
		return fmt.Errorf("contract_version %q is not supported (expected %q)", doc.ContractVersion, contractVersion)
	}
	if doc.DatasetVersion == "" {
		return fmt.Errorf("reference_dataset_version is required")
	}
	if doc.DatasetVersion != referenceDatasetVersion {
		return fmt.Errorf("reference_dataset_version %q does not match core's %q", doc.DatasetVersion, referenceDatasetVersion)
	}
	if doc.ModelInfo == nil || doc.ModelInfo.ExtractorModel == "" || doc.ModelInfo.AuditorModel == "" {
		return fmt.Errorf("model_info.extractor_model and model_info.auditor_model are required")
	}
	if doc.Fields == nil {
		return fmt.Errorf("fields is required")
	}
	return nil
}

// checkClassification validates the label enum and probability bounds:
// the three keys required by the contract, each in [0,1] and summing ~1.
func checkClassification(c *extractionClassification) error {
	if c == nil {
		return fmt.Errorf("classification is required")
	}
	switch c.Label {
	case "RFQ", "TARIFF_UPDATE", "OTHER":
	default:
		return fmt.Errorf("classification.label %q is not one of RFQ/TARIFF_UPDATE/OTHER", c.Label)
	}
	sum := 0.0
	for _, k := range []string{"RFQ", "TARIFF_UPDATE", "OTHER"} {
		p, ok := c.Probabilities[k]
		if !ok {
			return fmt.Errorf("classification.probabilities.%s is missing", k)
		}
		if p < 0 || p > 1 {
			return fmt.Errorf("classification.probabilities.%s %v is outside [0,1]", k, p)
		}
		sum += p
	}
	if math.Abs(sum-1) > probSumEpsilon {
		return fmt.Errorf("classification probabilities sum %v, expected ~1", sum)
	}
	return nil
}

// checkFields validates every FieldValue envelope and computes the gate
// input: the minimum auditor confidence across present fields.
func checkFields(fields map[string]fieldEnvelope) (*ExtractionReport, error) {
	report := &ExtractionReport{MinConfidence: 0, FieldCount: len(fields)}
	if len(fields) == 0 {
		return report, nil
	}
	min := math.MaxFloat64
	for name, fv := range fields {
		if !knownFields[name] {
			return nil, fmt.Errorf("fields.%s is not a contract field key", name)
		}
		if err := checkField(name, &fv); err != nil {
			return nil, err
		}
		if *fv.Confidence < min {
			min = *fv.Confidence
		}
	}
	report.MinConfidence = min
	return report, nil
}

// checkField validates one envelope's required keys and confidence bound.
func checkField(name string, fv *fieldEnvelope) error {
	if fv.Value == nil || fv.Evidence == nil || fv.Inferred == nil || fv.Confidence == nil || fv.Reasons == nil {
		return fmt.Errorf("fields.%s is missing required envelope keys (value/evidence/inferred/confidence/reasons)", name)
	}
	if !evidenceSourcePattern.MatchString(fv.Evidence.Source) || fv.Evidence.ExactQuote == "" {
		return fmt.Errorf("fields.%s.evidence requires source (EMAIL_BODY|ATTACHMENT:…) and exact_quote", name)
	}
	if *fv.Confidence < 0 || *fv.Confidence > 1 {
		return fmt.Errorf("fields.%s.confidence %v is outside [0,1]", name, *fv.Confidence)
	}
	return nil
}

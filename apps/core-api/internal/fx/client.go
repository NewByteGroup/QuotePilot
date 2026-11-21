// Package fx maintains app.fx_rates, the platform-global pinned FX rate
// table (migration 0011) the S3b rfq_price worker resolves into
// pricing.Input.FXRates.
//
// Two halves: Fetcher fetches Banco Central PTAX bulletins over HTTP —
// the only feed implemented today — and Syncer upserts the parsed rates
// through the caller's executor. LoadRates reads the pinned window the
// engine's newest-at-or-before resolution needs. CARRIER and CUSTOMS
// rows arrive through their own feeds; the table and the worker read
// path already accept them.
//
// The package holds no tenant context: fx_rates has no organization_id,
// so writes run on the plain qp_app pool outside any InTenantTx
// (AGENTS.md rule 3 scopes tenant rows, not platform data).
package fx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// defaultBaseURL is the Banco Central do Brasil Olinda OData service —
// the public PTAX feed, no credential.
const defaultBaseURL = "https://olinda.bcb.gov.br/olinda/servico/PTAX/versao/v1/odata"

// Bounds on one bulletin fetch (AGENTS.md: treat every remote response
// as untrusted): a 10s client timeout and a 1 MiB body cap — a 14-day
// window is ~70 rows, roughly 8 KiB of JSON.
const (
	httpTimeout  = 10 * time.Second
	bodyCapBytes = 1 << 20
)

// ptaxStampLayout parses dataHoraCotacao ("2025-11-14 13:11:28.202"),
// the bulletin's São Paulo wall time; the fractional second is variable
// in the feed (26.98 above comes back with two digits).
const ptaxStampLayout = "2006-01-02 15:04:05.999"

// isoDate mirrors the rate_date contract (yyyy-mm-dd).
const isoDate = "2006-01-02"

// currencyShape is the ISO-4217 shape the CHECK on fx_rates.currency
// enforces; requests outside it would 400 or pin a nonsense key.
var currencyShape = regexp.MustCompile(`^[A-Z]{3}$`)

// ptaxLocation anchors dataHoraCotacao in America/Sao_Paulo so rate_date
// is the bulletin date as BCB published it. Only the date portion
// survives into rate_date, so a container without tzdata falls back to
// UTC — ParseInLocation never shifts the wall date it parsed.
var ptaxLocation = func() *time.Location {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// Fetcher reads PTAX bulletins from the BCB Olinda API. The defaults hit
// the real endpoint; tests inject httptest via the options.
type Fetcher struct {
	client  *http.Client
	baseURL string
}

// Option customizes a Fetcher; used by tests to point at httptest.
type Option func(*Fetcher)

// WithHTTPClient swaps the 10s-timeout default client.
func WithHTTPClient(c *http.Client) Option {
	return func(f *Fetcher) { f.client = c }
}

// WithBaseURL swaps the BCB endpoint for a test server.
func WithBaseURL(u string) Option {
	return func(f *Fetcher) { f.baseURL = u }
}

// NewFetcher builds a Fetcher; no options means production defaults.
func NewFetcher(opts ...Option) *Fetcher {
	f := &Fetcher{
		client:  &http.Client{Timeout: httpTimeout},
		baseURL: defaultBaseURL,
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// bulletin is one usable PTAX row: the pinned rate plus the exact
// bulletin instant it came from (source_ref provenance).
type bulletin struct {
	day  time.Time       // rate_date: the bulletin's São Paulo date
	at   time.Time       // full dataHoraCotacao, for source_ref
	rate decimal.Decimal // cotacaoVenda — sell side, spec §6
}

// bulletins fetches one currency's window and reduces it to one row per
// bulletin date: the day's LAST bulletin wins. PTAX publishes ~5
// intraday bulletins; the closing one (fechamento, ~13:11 BRT) is the
// daily rate downstream consumers mean by "PTAX do dia", and picking the
// latest makes the (source, currency, rate_date) pin deterministic.
func (f *Fetcher) bulletins(ctx context.Context, currency string, since, until time.Time) ([]bulletin, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.ptaxURL(currency, since, until), nil)
	if err != nil {
		return nil, fmt.Errorf("fx: ptax request: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fx: ptax get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fx: ptax status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, bodyCapBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fx: ptax body: %w", err)
	}
	if len(raw) > bodyCapBytes {
		return nil, fmt.Errorf("fx: ptax body exceeds %d bytes", bodyCapBytes)
	}
	return parsePTAX(raw)
}

// ptaxURL builds the CotacaoMoedaPeriodo call. Verified 2025-11 against
// the live endpoint; the response shape is
// {"value":[{"cotacaoVenda":5.35050,"dataHoraCotacao":"2025-11-03
// 10:05:28.353"}, ...]} — ~5 bulletins per business day, "value" empty
// on weekends, holidays and unknown currencies. The @bind parameters
// must reach the query literal (url.Values would escape @ and $), and
// every dynamic part is validated ([A-Z]{3}, MM-DD-YYYY) so formatting
// is injection-free.
func (f *Fetcher) ptaxURL(currency string, since, until time.Time) string {
	return fmt.Sprintf(
		"%s/CotacaoMoedaPeriodo(moeda=@moeda,dataInicial=@di,dataFinalCotacao=@df)"+
			"?@moeda='%s'&@di='%s'&@df='%s'&$format=json&$select=cotacaoVenda,dataHoraCotacao",
		f.baseURL, currency,
		since.Format("01-02-2006"), until.Format("01-02-2006"))
}

// ptaxDoc mirrors the OData page. @odata.context is ignored; "value"
// carries every bulletin row in the window.
type ptaxDoc struct {
	Value []struct {
		Sell decimal.Decimal `json:"cotacaoVenda"`
		At   string          `json:"dataHoraCotacao"`
	} `json:"value"`
}

// parsePTAX decodes the page and keeps the latest bulletin per São Paulo
// date; zero/negative rates are feed noise and skipped (the CHECK on
// rate > 0 would refuse them anyway). A malformed timestamp is contract
// drift on a public feed — the caller surfaces it, never guesses.
func parsePTAX(raw []byte) ([]bulletin, error) {
	var doc ptaxDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("fx: ptax decode: %w", err)
	}
	byDay := make(map[string]bulletin, len(doc.Value))
	for _, row := range doc.Value {
		if !row.Sell.IsPositive() {
			continue
		}
		at, err := time.ParseInLocation(ptaxStampLayout, row.At, ptaxLocation)
		if err != nil {
			return nil, fmt.Errorf("fx: ptax dataHoraCotacao %q: %w", row.At, err)
		}
		key := at.Format(isoDate)
		if cur, ok := byDay[key]; !ok || at.After(cur.at) {
			day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
			byDay[key] = bulletin{day: day, at: at, rate: row.Sell}
		}
	}
	out := make([]bulletin, 0, len(byDay))
	for _, b := range byDay {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].day.Before(out[j].day) })
	return out, nil
}

package fx

// Parser and fetch tests for the BCB PTAX client — fixture JSON only,
// no live network (AGENTS.md: external I/O is faked). The fixture
// mirrors the verified Olinda shape: {"value":[{cotacaoVenda,
// dataHoraCotacao}]} with ~5 intraday bulletins per business day.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptaxFixture is a two-day window with five intraday bulletins on the
// first day and two on the second — the closing bulletin must win.
const ptaxFixture = `{"@odata.context":"https://was-p.bcnet.bcb.gov.br/olinda/servico/PTAX/versao/v1/odata$metadata#_CotacaoMoedaPeriodo(cotacaoVenda,dataHoraCotacao)",
"value":[
  {"cotacaoVenda":5.35050,"dataHoraCotacao":"2025-11-03 10:05:28.353"},
  {"cotacaoVenda":5.35040,"dataHoraCotacao":"2025-11-03 11:08:24.576"},
  {"cotacaoVenda":5.34610,"dataHoraCotacao":"2025-11-03 12:06:25.517"},
  {"cotacaoVenda":5.35500,"dataHoraCotacao":"2025-11-03 13:06:25.322"},
  {"cotacaoVenda":5.35050,"dataHoraCotacao":"2025-11-03 13:06:25.333"},
  {"cotacaoVenda":5.38450,"dataHoraCotacao":"2025-11-04 10:04:26.385"},
  {"cotacaoVenda":5.38460,"dataHoraCotacao":"2025-11-04 13:07:26.98"}
]}`

// TestParsePTAXClosingWins: one row per bulletin date, and the pinned
// rate is the day's LAST bulletin — the closing PTAX.
func TestParsePTAXClosingWins(t *testing.T) {
	rows, err := parsePTAX([]byte(ptaxFixture))
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "2025-11-03", rows[0].day.Format(isoDate))
	assert.Equal(t, "5.3505", rows[0].rate.String(), "13:06:25.333 beats 13:06:25.322")
	assert.Equal(t, "2025-11-04", rows[1].day.Format(isoDate))
	assert.Equal(t, "5.3846", rows[1].rate.String())
	assert.Equal(t, "2025-11-04 13:07:26.98", rows[1].at.Format(ptaxStampLayout),
		"source_ref provenance keeps the full bulletin instant")
}

// TestParsePTAXSkipsNonPositive: zero and negative cotacaoVenda rows are
// feed noise — the CHECK on rate > 0 would refuse them anyway.
func TestParsePTAXSkipsNonPositive(t *testing.T) {
	doc := `{"value":[
		{"cotacaoVenda":0,"dataHoraCotacao":"2025-11-03 10:05:28.353"},
		{"cotacaoVenda":-1.5,"dataHoraCotacao":"2025-11-03 11:08:24.576"},
		{"cotacaoVenda":5.3,"dataHoraCotacao":"2025-11-03 13:06:25.333"}
	]}`
	rows, err := parsePTAX([]byte(doc))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "5.3", rows[0].rate.String())
}

// TestParsePTAXEmpty: weekends, holidays and unknown currencies come
// back 200 with an empty "value" — an empty window, not an error.
func TestParsePTAXEmpty(t *testing.T) {
	for _, doc := range []string{`{"value":[]}`, `{}`, `{"value":null}`} {
		rows, err := parsePTAX([]byte(doc))
		require.NoError(t, err, doc)
		assert.Empty(t, rows, doc)
	}
}

// TestParsePTAXMalformed: a timestamp outside the feed's layout is
// contract drift — error, never a guessed date.
func TestParsePTAXMalformed(t *testing.T) {
	doc := `{"value":[{"cotacaoVenda":5.3,"dataHoraCotacao":"03/11/2025 13:06"}]}`
	_, err := parsePTAX([]byte(doc))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dataHoraCotacao")

	_, err = parsePTAX([]byte(`not json`))
	require.Error(t, err)
}

// TestBulletinsURL pins the request the Fetcher builds: the function
// path, the @bind parameter names the OData endpoint needs literal and
// the MM-DD-YYYY date shape.
func TestBulletinsURL(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"value":[]}`)
	}))
	defer srv.Close()

	f := NewFetcher(WithHTTPClient(srv.Client()), WithBaseURL(srv.URL))
	_, err := f.bulletins(context.Background(), "USD",
		time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 11, 14, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	assert.Equal(t,
		"/CotacaoMoedaPeriodo(moeda=@moeda,dataInicial=@di,dataFinalCotacao=@df)",
		gotPath)
	assert.Equal(t,
		"@moeda='USD'&@di='11-01-2025'&@df='11-14-2025'&$format=json&$select=cotacaoVenda,dataHoraCotacao",
		gotQuery)
}

// TestBulletinsEndToEnd: fixture JSON through the httptest transport
// comes out as pinned bulletins — the whole fetch path, no live BCB.
func TestBulletinsEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, ptaxFixture)
	}))
	defer srv.Close()

	f := NewFetcher(WithHTTPClient(srv.Client()), WithBaseURL(srv.URL))
	rows, err := f.bulletins(context.Background(), "USD",
		time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 11, 14, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

// TestBulletinsErrors: a non-200 status and an oversized body both fail
// — the worker then degrades to a warn, never partial data.
func TestBulletinsErrors(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()
		f := NewFetcher(WithHTTPClient(srv.Client()), WithBaseURL(srv.URL))
		_, err := f.bulletins(context.Background(), "USD", time.Now(), time.Now())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "502")
	})

	t.Run("body cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, strings.Repeat("x", bodyCapBytes+8))
		}))
		defer srv.Close()
		f := NewFetcher(WithHTTPClient(srv.Client()), WithBaseURL(srv.URL))
		_, err := f.bulletins(context.Background(), "USD", time.Now(), time.Now())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeds")
	})
}

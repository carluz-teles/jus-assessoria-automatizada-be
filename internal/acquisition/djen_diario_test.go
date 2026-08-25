package acquisition

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// diarioItem builds a comunicação JSON for the by-tribunal ingestion read, with the
// recipient advogados the local match keys off.
func diarioItem(hash, tribunal, date string, oabs ...[2]string) json.RawMessage {
	advs := make([]map[string]any, 0, len(oabs))
	for _, o := range oabs {
		advs = append(advs, map[string]any{
			"advogado": map[string]any{"nome": "ADV " + o[0], "numero_oab": o[0], "uf_oab": o[1]},
		})
	}
	raw, _ := json.Marshal(map[string]any{
		"hash":                  hash,
		"siglaTribunal":         tribunal,
		"numero_processo":       "1000",
		"data_disponibilizacao": date,
		"destinatarioadvogados": advs,
	})
	return raw
}

// TestDJENConnector_FetchDiario walks a tribunal's diário: it must filter by
// siglaTribunal (not numeroOab), paginate to the first short page, and return every
// item across pages.
func TestDJENConnector_FetchDiario(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("siglaTribunal") != "TJSP" || q.Get("numeroOab") != "" {
			t.Errorf("query = %v, want siglaTribunal=TJSP and no numeroOab", q.Encode())
		}
		switch q.Get("pagina") {
		case "1":
			_, _ = w.Write(djenBody(t, djenItem("A"), djenItem("B"))) // full page (size 2)
		case "2":
			_, _ = w.Write(djenBody(t, djenItem("C"))) // short page → stop
		default:
			_, _ = w.Write(djenBody(t))
		}
	}))
	defer srv.Close()

	c := NewDJENConnector(WithDJENBaseURL(srv.URL), WithDJENRatePerMinute(6000000), WithDJENPageSize(2))
	items, err := c.FetchDiario(context.Background(), "TJSP", "2025-08-08", "2025-08-08")
	if err != nil {
		t.Fatalf("FetchDiario: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3 (A,B across page 1 + C on page 2)", len(items))
	}
}

// TestPublicationParamsFromItems maps raw diário items to store params: the hash /
// court / process / date carry through, recipient OAB keys are normalized+deduped,
// an item without a hash is skipped, and one without advogados yields an empty set.
func TestPublicationParamsFromItems(t *testing.T) {
	t.Parallel()

	items := []json.RawMessage{
		diarioItem("h1", "TJSP", "2025-08-08", [2]string{"347019", "sp"}, [2]string{"347019", "SP"}, [2]string{"198988", "MG"}),
		diarioItem("", "TJRJ", "2025-08-08", [2]string{"1", "RJ"}), // no hash → skipped
		diarioItem("h3", "TRE-SP", "2025-08-08"),                   // no advogados → empty keys
	}

	got, err := publicationParamsFromItems(items)
	if err != nil {
		t.Fatalf("publicationParamsFromItems: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d params, want 2 (the hash-less item is skipped)", len(got))
	}

	h1 := got[0]
	if h1.Hash != "h1" || h1.Court != "TJSP" || h1.CNJNumber != "1000" {
		t.Errorf("h1 = %+v, want hash h1 / court TJSP / cnj 1000", h1)
	}
	if h1.MadeAvailableAt.Year() != 2025 || h1.MadeAvailableAt.Month() != 8 || h1.MadeAvailableAt.Day() != 8 {
		t.Errorf("h1 made_available_at = %v, want 2025-08-08", h1.MadeAvailableAt)
	}
	// "347019/sp" and "347019/SP" normalize to the same key (deduped); "198988/MG" distinct.
	if len(h1.RecipientOABs) != 2 || h1.RecipientOABs[0] != "347019|SP" || h1.RecipientOABs[1] != "198988|MG" {
		t.Errorf("h1 recipient_oabs = %v, want [347019|SP 198988|MG]", h1.RecipientOABs)
	}

	h3 := got[1]
	if h3.Hash != "h3" || h3.RecipientOABs == nil || len(h3.RecipientOABs) != 0 {
		t.Errorf("h3 = %+v, want hash h3 with empty non-null recipient_oabs", h3)
	}
}

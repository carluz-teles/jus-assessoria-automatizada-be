package acquisition

import (
	"strings"
	"testing"
)

// TestTribunais pins the DJEN court registry: the exact set the national ingestion
// sweeps. A missing court means a whole branch/state is never fetched, so the count,
// the validated sigla spellings, and the UF scoping are all asserted.
func TestTribunais(t *testing.T) {
	t.Parallel()

	// 27 estaduais + 3 militares + 27 eleitorais + 6 TRFs + 24 TRTs + 4 superiores.
	const want = 27 + 3 + 27 + 6 + 24 + 4
	if len(tribunais) != want {
		t.Fatalf("len(tribunais) = %d, want %d", len(tribunais), want)
	}

	bySigla := make(map[string]tribunal, len(tribunais))
	for _, tr := range tribunais {
		if tr.sigla == "" {
			t.Errorf("tribunal com sigla vazia: %+v", tr)
		}
		if tr.sigla != strings.ToUpper(tr.sigla) {
			t.Errorf("sigla não-uppercase: %q", tr.sigla)
		}
		if _, dup := bySigla[tr.sigla]; dup {
			t.Errorf("sigla duplicada: %q", tr.sigla)
		}
		bySigla[tr.sigla] = tr
	}

	// Grafias validadas contra a API viva (2026-08) + exclusões.
	cases := []struct {
		sigla   string
		uf      string
		present bool
	}{
		{sigla: "TJSP", uf: "SP", present: true},
		{sigla: "TJDFT", uf: "DF", present: true}, // DF é TJDFT, não TJDF
		{sigla: "TRE-SP", uf: "SP", present: true},
		{sigla: "TJMSP", uf: "SP", present: true},
		{sigla: "TRF3", uf: "", present: true},
		{sigla: "TRT15", uf: "", present: true},
		{sigla: "STJ", uf: "", present: true},
		{sigla: "STM", uf: "", present: true},
		{sigla: "TST", uf: "", present: true},
		{sigla: "TSE", uf: "", present: true},
		{sigla: "TJDF", present: false},  // grafia errada
		{sigla: "TRESP", present: false}, // eleitoral é hifenizado
		{sigla: "STF", present: false},   // 500 no endpoint
	}
	for _, c := range cases {
		t.Run(c.sigla, func(t *testing.T) {
			t.Parallel()
			tr, ok := bySigla[c.sigla]
			if ok != c.present {
				t.Fatalf("%q present = %v, want %v", c.sigla, ok, c.present)
			}
			if ok && tr.uf != c.uf {
				t.Errorf("%q uf = %q, want %q", c.sigla, tr.uf, c.uf)
			}
		})
	}

	// Cobertura: todo estado + DF tem um TJ e um TRE.
	for _, uf := range ufsBR {
		tj := "TJ" + uf
		if uf == "DF" {
			tj = "TJDFT"
		}
		if _, ok := bySigla[tj]; !ok {
			t.Errorf("faltou o TJ de %s (%q)", uf, tj)
		}
		if _, ok := bySigla["TRE-"+uf]; !ok {
			t.Errorf("faltou o TRE de %s (TRE-%s)", uf, uf)
		}
	}
}

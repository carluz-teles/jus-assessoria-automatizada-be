package eproc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubPartiesHTML mirrors the CONFIRMED shape of the real capa's
// #tblPartesERepresentantes: data-parte="AUTOR"/"REU" party blocks, each with a
// spnCpfParte* span for the CPF/CNPJ and infraTooltipMostrar('ADVOGADO',…) links whose
// content is the OAB (UF+numero) and whose name precedes them. It deliberately includes
// two traps the parser must NOT fall for:
//
//   - a <style> block BEFORE the table using data-parte inside a CSS selector
//     (span.infraEventoPrazoParte[data-parte="AUTOR"]) — proves the search is SCOPED to
//     the parties table, not the whole document.
//   - accented values ("MURILO DE PÁULA", "Cristóvão") — proves the fixture drives the
//     same UTF-8 text path the real (ISO-8859-1) page does after parseHTMLDocument.
//
// It also covers a réu WITHOUT a CPF span and WITHOUT any advogado (the incomplete-row
// cases the parser must tolerate rather than skip or crash on).
const stubPartiesHTML = `<html><head>
<style>
span.infraEventoPrazoParte[data-parte="AUTOR"] { color: green; }
span.infraEventoPrazoParte[data-parte="REU"] { color: red; }
</style>
</head><body>
<div id="tblEventos" data-parte="REU">docket noise carrying data-parte, outside the parties table</div>
<table id="tblPartesERepresentantes">
  <tr>
    <td>
      <span data-parte="AUTOR">
        <span id="spnNomeParteAutor0">MURILO DE PÁULA BALDAN</span>
        ( <span id="spnCpfParteAutor0">284.669.278-59</span> ) - Pessoa Física
        PAULO SERGIO DE OLIVEIRA SOUZA<a onmouseover="infraTooltipMostrar('ADVOGADO','Tipo de Usuário')" href="#">SP321511</a>
        <div class="sr-only">Tipo de Usuário: ADVOGADO</div>
        LUAN GOMES<a href="#">SP999000</a>
        <div class="sr-only">Tipo de Usuário: ADVOGADO</div>
      </span>
    </td>
  </tr>
  <tr>
    <td>
      <span data-parte="REU">
        <span id="spnNomeParteReu0">RITA MÁRCIA MONTEIRO SEZEFREDO</span>
      </span>
    </td>
  </tr>
</table>
</body></html>`

func TestParsePartiesHTML_ExtractsAutorReuCpfAndCounsels(t *testing.T) {
	t.Parallel()

	proc, err := parseProcessHTML([]byte(stubPartiesHTML))
	require.NoError(t, err)
	require.Len(t, proc.Parties, 2, "one autor + one réu")

	// Authors come before réus.
	autor := proc.Parties[0]
	assert.Equal(t, "AUTOR", autor.Role)
	assert.Equal(t, "MURILO DE PÁULA BALDAN", autor.Name, "accented name survives the UTF-8 path")
	assert.Equal(t, "284.669.278-59", autor.Document)
	assert.Equal(t, autor.Document, autor.RawDocument, "RawDocument mirrors Document")

	require.Len(t, autor.Counsels, 2)
	assert.Equal(t, Counsel{Name: "PAULO SERGIO DE OLIVEIRA SOUZA", OAB: "321511", UF: "SP"}, autor.Counsels[0])
	assert.Equal(t, Counsel{Name: "LUAN GOMES", OAB: "999000", UF: "SP"}, autor.Counsels[1],
		"an advogado flagged only by the sr-only div (no ADVOGADO tooltip) still counts")

	reu := proc.Parties[1]
	assert.Equal(t, "REU", reu.Role)
	assert.Equal(t, "RITA MÁRCIA MONTEIRO SEZEFREDO", reu.Name)
	assert.Empty(t, reu.Document, "réu block carried no CPF span")
	assert.Empty(t, reu.Counsels, "réu block carried no advogado")
}

func TestParsePartiesHTML_IgnoresDataParteOutsideTheTable(t *testing.T) {
	t.Parallel()

	proc, err := parseProcessHTML([]byte(stubPartiesHTML))
	require.NoError(t, err)

	// The <style> selector and the #tblEventos div both carry data-parte but live
	// OUTSIDE #tblPartesERepresentantes — exactly 2 parties means neither leaked in.
	require.Len(t, proc.Parties, 2)
	for _, p := range proc.Parties {
		assert.NotEmpty(t, p.Name, "no decoration (style/docket) was mistaken for a party")
	}
}

func TestParsePartiesHTML_CpfFromParenthesizedTextFallback(t *testing.T) {
	t.Parallel()

	// A party block with NO spnCpfParte* span — the CPF lives only in the
	// "( ... ) - Pessoa Física" text the parser falls back to.
	const html = `<html><body><table id="tblPartesERepresentantes">
	<tr><td><span data-parte="AUTOR">JOÃO DA SILVA ( 111.222.333-44 ) - Pessoa Física</span></td></tr>
	</table></body></html>`

	proc, err := parseProcessHTML([]byte(html))
	require.NoError(t, err)
	require.Len(t, proc.Parties, 1)
	assert.Equal(t, "JOÃO DA SILVA", proc.Parties[0].Name)
	assert.Equal(t, "111.222.333-44", proc.Parties[0].Document)
}

func TestParsePartiesHTML_CnpjDocumentAndForeignOAB(t *testing.T) {
	t.Parallel()

	// A réu that is a legal entity (CNPJ) with an advogado whose OAB rendering does
	// NOT match the UF+digits shape — it is carried whole as OAB with an empty UF
	// rather than guessed at.
	const html = `<html><body><table id="tblPartesERepresentantes">
	<tr><td><span data-parte="REU">
	  <span id="spnNomeParteReu0">ACME LTDA</span>
	  ( <span id="spnCpfParteReu0">12.345.678/0001-95</span> ) - Pessoa Jurídica
	  DR ESTRANGEIRO<a onmouseover="infraTooltipMostrar('ADVOGADO','x')" href="#">OAB-INTL</a>
	</span></td></tr>
	</table></body></html>`

	proc, err := parseProcessHTML([]byte(html))
	require.NoError(t, err)
	require.Len(t, proc.Parties, 1)
	assert.Equal(t, "12.345.678/0001-95", proc.Parties[0].Document)
	require.Len(t, proc.Parties[0].Counsels, 1)
	assert.Equal(t, Counsel{Name: "DR ESTRANGEIRO", OAB: "OAB-INTL", UF: ""}, proc.Parties[0].Counsels[0])
}

func TestParsePartiesHTML_NoTableYieldsEmpty(t *testing.T) {
	t.Parallel()

	proc, err := parseProcessHTML([]byte(`<html><body>no parties table here</body></html>`))
	require.NoError(t, err)
	assert.Empty(t, proc.Parties)
	assert.NotNil(t, proc.Parties, "empty, never nil — callers range freely")
}

func TestSplitOAB(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		wantUF  string
		wantOAB string
	}{
		{name: "uf plus number", raw: "SP321511", wantUF: "SP", wantOAB: "321511"},
		{name: "lowercase uf normalized", raw: "rj12345", wantUF: "RJ", wantOAB: "12345"},
		{name: "uf and number with space", raw: "MG 8080", wantUF: "MG", wantOAB: "8080"},
		{name: "no match carried whole", raw: "OAB-INTL", wantUF: "", wantOAB: "OAB-INTL"},
		{name: "empty", raw: "", wantUF: "", wantOAB: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			is := assert.New(t)
			uf, oab := splitOAB(tt.raw)
			is.Equal(tt.wantUF, uf)
			is.Equal(tt.wantOAB, oab)
		})
	}
}

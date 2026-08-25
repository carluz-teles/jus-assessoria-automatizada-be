package draft

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/chromedp/chromedp"
)

// chromedpFilingGateway implementa FilingGateway via RPA headless no e-SAJ
// (peticao.v.tjsp.jus.br). É a única fronteira com o site do tribunal — todo o
// resto do slice (estado, idempotência, eventos) é determinístico e testável.
//
// ATENÇÃO (área que exige verificação manual em staging): os seletores CSS
// abaixo foram escritos com base na estrutura conhecida do e-SAJ v1 (TJSP). O
// e-SAJ muda frequentemente (iframes, selects dinâmicos, captchas ocasionais),
// então este adapter precisa de um passe de calibragem contra o ambiente de
// homologação antes de ir pra produção. Os pontos de captura de tela (e o
// número de protocolo lido no fim) são a âncora de depuração.
type chromedpFilingGateway struct {
	headless bool
}

// NewChromedpFilingGateway constrói o gateway real (chromedp). headless=false
// útil pra depuração local com o Chrome visível.
func NewChromedpFilingGateway(headless bool) FilingGateway {
	return &chromedpFilingGateway{headless: headless}
}

const esajBaseURL = "https://peticao.v.tjsp.jus.br/peticionamento/"

// Protocol executa o fluxo e-SAJ e devolve o número de protocolo + screenshots.
func (g *chromedpFilingGateway) Protocol(ctx context.Context, req FilingRequest) (*FilingResult, error) {
	// 1) Chromium headless.
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", g.headless),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()
	// Timeout global de 5min pra todo o fluxo RPA — evita que um e-SAJ lento/
	// preso deixe a goroutine (e a sessão do tribunal) pendurada para sempre.
	timeoutCtx, cancelTimeout := context.WithTimeout(allocCtx, 5*time.Minute)
	defer cancelTimeout()
	browserCtx, cancelBrowser := chromedp.NewContext(timeoutCtx)
	defer cancelBrowser()

	// shots acumula os buffers PNG de cada passo do RPA (variável local: o
	// gateway é singleton, então nunca pode reter estado entre Protocol()s).
	// O worker persiste esses bytes no storage e grava os keys na attempt.
	var shots [][]byte
	shot := func(name string) {
		var buf []byte
		if err := chromedp.Run(browserCtx, chromedp.CaptureScreenshot(&buf)); err == nil && len(buf) > 0 {
			shots = append(shots, buf)
		}
	}

	// 2) Login e-SAJ (advogado: usuário = OAB/CPF, senha = req.Password).
	loginActions := []chromedp.Action{
		chromedp.Navigate(esajBaseURL),
		// Seletores estimados — calibrar em staging.
		chromedp.WaitVisible(`input[name="loginUsuario"]`, chromedp.ByQuery),
		chromedp.SendKeys(`input[name="loginUsuario"]`, req.Login, chromedp.ByQuery),
		chromedp.SendKeys(`input[name="senhaUsuario"]`, req.Password, chromedp.ByQuery),
		chromedp.Click(`input[name="botaoLogin"]`, chromedp.ByQuery),
		chromedp.Sleep(2 * time.Second),
	}
	if err := chromedp.Run(browserCtx, loginActions...); err != nil {
		shot("login-falhou")
		return nil, fmt.Errorf("esaj login: %w", err)
	}
	shot("apos-login")

	// 3) Abrir "Peticionar".
	openActions := []chromedp.Action{
		chromedp.Navigate(esajBaseURL + "abrirPeticionamento.do"),
		chromedp.WaitVisible(`#comarca`, chromedp.ByQuery),
		chromedp.SendKeys(`#comarca`, req.Comarca, chromedp.ByQuery),
		chromedp.Sleep(1 * time.Second),
		chromedp.SendKeys(`#vara`, req.Vara, chromedp.ByQuery),
		chromedp.Sleep(1 * time.Second),
		chromedp.SendKeys(`#classe`, req.PetitionType, chromedp.ByQuery),
	}
	if err := chromedp.Run(browserCtx, openActions...); err != nil {
		shot("peticionar-falhou")
		return nil, fmt.Errorf("esaj abrir peticionamento: %w", err)
	}
	shot("peticionar-form")

	// 4) Upload do PDF assinado (snapshot congelado no clique).
	tmp, err := os.CreateTemp("", "filing-*.pdf")
	if err != nil {
		return nil, fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(req.PDF); err != nil {
		return nil, fmt.Errorf("temp write: %w", err)
	}
	tmp.Close()
	uploadActions := []chromedp.Action{
		chromedp.SetUploadFiles(`input[type="file"][name="arquivo"]`, []string{tmp.Name()}, chromedp.ByQuery),
		chromedp.Sleep(2 * time.Second),
	}
	if err := chromedp.Run(browserCtx, uploadActions...); err != nil {
		shot("upload-falhou")
		return nil, fmt.Errorf("esaj upload pdf: %w", err)
	}
	shot("apos-upload")

	// 5) Preencher polos (partes) + confirmar.
	partyActions := []chromedp.Action{
		chromedp.SendKeys(`#poloAtivo`, req.PartyNames, chromedp.ByQuery),
		chromedp.Sleep(500 * time.Millisecond),
		chromedp.Click(`input[name="botaoConfirmar"]`, chromedp.ByQuery),
		chromedp.Sleep(3 * time.Second),
	}
	if err := chromedp.Run(browserCtx, partyActions...); err != nil {
		shot("confirmar-falhou")
		return nil, fmt.Errorf("esaj confirmar: %w", err)
	}
	shot("apos-confirmar")

	// 6) Ler número de protocolo da tela de recibo.
	var protocolNumber string
	readActions := []chromedp.Action{
		chromedp.WaitVisible(`#numeroProtocolo`, chromedp.ByQuery),
		chromedp.Text(`#numeroProtocolo`, &protocolNumber, chromedp.ByQuery),
	}
	if err := chromedp.Run(browserCtx, readActions...); err != nil {
		shot("protocolo-falhou")
		return nil, fmt.Errorf("esaj ler protocolo: %w", err)
	}
	shot("protocolado")

	return &FilingResult{FilingNumber: protocolNumber, Screenshots: shots}, nil
}

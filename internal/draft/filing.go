package draft

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hibiken/asynq"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/storage"
)

// ── FilingAttempt (Fatia 1 — peticionamento automático) ──────────────────────

// Status da tentativa de protocolo automático (filing_attempt.status).
const (
	StatusEnfileirado  = "ENFILEIRADO"
	StatusProtocolando = "PROTOCOLANDO"
	StatusProtocolado  = "PROTOCOLADO"
	StatusFalhou       = "FALHOU"
)

// FilingAttempt é uma tentativa de protocolo automático via e-SAJ. Append-only:
// cada clique de "Protocolar" gera uma. O snapshot do PDF (PdfStorageKey +
// PdfSha256) congela o bytes protocolado no momento do clique.
type FilingAttempt struct {
	ID             string
	DraftID        string
	PetitionID     string
	Status         string
	PdfStorageKey  string
	PdfSha256      string
	RequestedBy    string
	RequestedAt    time.Time
	StartedAt      time.Time
	FinishedAt     time.Time
	FailureReason  string
	FilingNumber   string
	ScreenshotKeys []string
}

// ── FilingGateway (port do adapter e-SAJ, chromedp) ──────────────────────────

// FilingRequest é o input do gateway: o PDF assinado congelado + as credenciais
// decifradas (em memória). CNJ dá contexto pro preenchimento do formulário.
type FilingRequest struct {
	TenantID     string
	DraftID      string
	PDF          []byte
	Login        string
	Password     string
	CNJ          string // número do processo (CNJ) alvo no e-SAJ
	Comarca      string
	Vara         string
	PetitionType string // classe processual
	PartyNames   string // nomes dos polos (texto livre)
}

// FilingResult é o retorno do gateway: o recibo do tribunal (número de protocolo)
// + screenshots da sessão e-SAJ (para auditoria/debug).
type FilingResult struct {
	Receipt      map[string]any
	FilingNumber string
	Screenshots  [][]byte
}

// FilingGateway é a porta do RPA e-SAJ (adapter chromedp em prod). O worker
// injeta a implementação; nos testes usa-se um fake.
type FilingGateway interface {
	Protocol(ctx context.Context, req FilingRequest) (*FilingResult, error)
}

// ── ApproveFiling (handler → enqueue) ────────────────────────────────────────

// ApproveFilingCommand é o input do POST /v1/pecas/:id/filing/approve.
type ApproveFilingCommand struct {
	TenantID string
	DraftID  string
	UserID   string // principal — dono da credencial e-SAJ e quem solicitou
}

// ApproveFilingResult devolve o id da tentativa. IsIdempotent=true quando já
// havia uma tentativa ativa (duplo-clique → mesma tentativa, critério 3).
type ApproveFilingResult struct {
	FilingAttemptID string
	Status          string
	IsIdempotent    bool
}

// ApproveFiling implementa POST /v1/pecas/:id/filing/approve. NUNCA auto-file sem
// este clique explícito. Em UMA tx: guard SIGNED + snapshot do PDF assinado +
// insert filing_attempt ENFILEIRADO + outbox filing.enqueued. A idempotência é
// dada pelo unique parcial (draft_id ativo) + pelo check de tentativa ativa.
func (uc *UseCase) ApproveFiling(ctx context.Context, cmd ApproveFilingCommand) (*ApproveFilingResult, error) {
	if uc.pdfStorage == nil {
		return nil, ErrPDFStorageUnavailable
	}

	// 1) Carrega o draft (guard de tenant) e valida SIGNED.
	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		d, e := uc.rw.GetDraftByID(ctx, tx, cmd.TenantID, cmd.DraftID)
		if e != nil {
			return e
		}
		if d.Status != StatusSigned {
			return ErrFilingNotSigned
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 2) Consentimento: precisa de credencial e-SAJ ativa para o usuário.
	if err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		_, e := uc.rw.GetActiveEsajCredential(ctx, tx, cmd.TenantID, cmd.UserID)
		return e
	}); err != nil {
		if isNoRows(err) {
			return nil, ErrFilingConsentRequired
		}
		return nil, err
	}

	// 3) Idempotência: se já há tentativa ativa, devolve a existente (click duplo).
	active, err := func() (*FilingAttempt, error) {
		var a *FilingAttempt
		e := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
			aa, ee := uc.rw.GetActiveFilingAttempt(ctx, tx, cmd.TenantID, cmd.DraftID)
			if ee != nil {
				return ee
			}
			a = aa
			return nil
		})
		return a, e
	}()
	if err != nil {
		return nil, err
	}
	if active != nil {
		return &ApproveFilingResult{
			FilingAttemptID: active.ID,
			Status:          active.Status,
			IsIdempotent:    true,
		}, nil
	}

	// 4) Snapshot do PDF assinado: congela os bytes no momento do clique, assim
	// edição pós-aprovação não altera o PDF protocolado (critério 7).
	snapshotKey := storage.NewKey(cmd.TenantID, fmt.Sprintf("pecas/%s/filing-snapshot", cmd.DraftID))
	sha, err := uc.snapshotSignedPDF(ctx, cmd.TenantID, cmd.DraftID, snapshotKey)
	if err != nil {
		return nil, err
	}

	// 5) Tx: insert ENFILEIRADO + outbox filing.enqueued.
	var result *ApproveFilingResult
	err = uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		attempt, e := uc.rw.InsertFilingAttempt(ctx, tx, cmd.TenantID, cmd.DraftID, snapshotKey, sha, cmd.UserID)
		if e != nil {
			// unique parcial (draft ativo) → outra thread/click já enfileirou.
			if errors.Is(e, ErrFilingAttemptConflict) {
				return errFilingAlreadyEnqueued
			}
			return e
		}
		if uc.outbox != nil {
			ev := newFilingEnqueued(attempt.DraftID, cmd.TenantID, attempt.ID)
			if e := uc.outbox.Publish(ctx, tx, ev); e != nil {
				return e
			}
		}
		result = &ApproveFilingResult{
			FilingAttemptID: attempt.ID,
			Status:          attempt.Status,
			IsIdempotent:    false,
		}
		return nil
	})
	if errors.Is(err, errFilingAlreadyEnqueued) {
		// Outro caminho ganhou a corrida: devolve a tentativa ativa existente.
		return uc.filingAlreadyEnqueued(ctx, cmd.TenantID, cmd.DraftID)
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// filingAlreadyEnqueued devolve a tentativa ativa existente (idempotência de
// corrida no insert).
func (uc *UseCase) filingAlreadyEnqueued(ctx context.Context, tenantID, draftID string) (*ApproveFilingResult, error) {
	var active *FilingAttempt
	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		a, e := uc.rw.GetActiveFilingAttempt(ctx, tx, tenantID, draftID)
		if e != nil {
			return e
		}
		active = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	if active == nil {
		// A corrida resolveu em delete? Improvável; trata como conflito genérico.
		return nil, ErrFilingAttemptConflict
	}
	return &ApproveFilingResult{
		FilingAttemptID: active.ID,
		Status:          active.Status,
		IsIdempotent:    true,
	}, nil
}

// snapshotSignedPDF copia o PDF assinado (draft.signed_pdf_key) para snapshotKey
// e devolve o sha256 hex. Falha se a peça não tiver PDF assinado.
func (uc *UseCase) snapshotSignedPDF(ctx context.Context, tenantID, draftID, snapshotKey string) (string, error) {
	var srcKey string
	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		v, e := uc.rw.GetDraftDetail(ctx, tx, tenantID, draftID)
		if e != nil {
			return e
		}
		if v.SignedPDFKey == "" {
			return ErrFilingNotSigned
		}
		srcKey = v.SignedPDFKey
		return nil
	})
	if err != nil {
		return "", err
	}
	pdf, err := uc.pdfStorage.GetBytes(ctx, srcKey)
	if err != nil {
		return "", apperr.NewInfra("ler pdf assinado", err)
	}
	if err := uc.pdfStorage.PutBytes(ctx, snapshotKey, "application/pdf", pdf); err != nil {
		return "", apperr.NewInfra("snapshot pdf", err)
	}
	sum := sha256.Sum256(pdf)
	return hex.EncodeToString(sum[:]), nil
}

// GetFilingStatus devolve a tentativa de protocolo da peça (ou 404 se nenhuma).
func (uc *UseCase) GetFilingStatus(ctx context.Context, tenantID, draftID string) (*FilingAttempt, error) {
	// Última tentativa do draft (qualquer status, inclusive PROTOCOLADO/FALHOU),
	// ordenada por requested_at DESC — o endpoint de status reflete o estado
	// terminal, não só as tentativas ativas. Miss → 404 (handler → "não iniciado").
	var out *FilingAttempt
	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		a, e := uc.rw.GetLatestFilingAttempt(ctx, tx, draftID)
		if e != nil {
			return e
		}
		out = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, ErrFilingAttemptNotFound
	}
	return out, nil
}

// errFilingAlreadyEnqueued é um sentinel interno (não escapa pro cliente — o
// handler mapeia como a tentativa existente).
var errFilingAlreadyEnqueued = errors.New("filing already enqueued")

// ── FilingUseCase (worker-filing) ────────────────────────────────────────────

// FilingUseCase é o consumidor assíncrono do filing.enqueued: roda o RPA e-SAJ,
// cria a petition em caso de sucesso e publica filing.succeeded/failed.
type FilingUseCase struct {
	repo    database.UnitOfWork
	rw      Repository
	now     func() time.Time
	outbox  OutboxPublisher
	storage PDFStorage
	vault   SecretVault
	gateway FilingGateway
}

// NewFilingUseCase wires o use case do worker. Os ports storage/vault/gateway são
// injetados por options no composition root.
func NewFilingUseCase(uow database.UnitOfWork, rw Repository, opts ...FilingOption) *FilingUseCase {
	uc := &FilingUseCase{repo: uow, rw: rw, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// FilingOption configura o FilingUseCase.
type FilingOption func(*FilingUseCase)

func WithFilingOutbox(ob OutboxPublisher) FilingOption {
	return func(uc *FilingUseCase) { uc.outbox = ob }
}
func WithFilingStorage(s PDFStorage) FilingOption { return func(uc *FilingUseCase) { uc.storage = s } }
func WithFilingVault(v SecretVault) FilingOption  { return func(uc *FilingUseCase) { uc.vault = v } }
func WithFilingGateway(g FilingGateway) FilingOption {
	return func(uc *FilingUseCase) { uc.gateway = g }
}

// Register monta o handler filing.enqueued no mux do worker-filing.
func (uc *FilingUseCase) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeFilingEnqueued, uc.handleFilingEnqueued)
}

func (uc *FilingUseCase) handleFilingEnqueued(ctx context.Context, t *asynq.Task) error {
	ev, err := events.Decode[FilingEnqueued](t)
	if err != nil {
		return err
	}
	if err := uc.OnFilingEnqueued(ctx, ev); err != nil {
		if isFilingTerminal(err) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	return nil
}

// isFilingTerminal: erros de validação/consentimento não melhoram no retry.
func isFilingTerminal(err error) bool {
	ae, ok := apperr.From(err)
	if !ok {
		return false
	}
	return ae.Kind == apperr.KindInvalid || ae.Kind == apperr.KindNotFound
}

// OnFilingEnqueued roda o protocolo automático. Idempotente sob retry/redelivery:
// tentativa fora de ENFILEIRADO → no-op; a guarda de status garante exatamente
// uma transição PROTOCOLANDO por tentativa (critério 3).
func (uc *FilingUseCase) OnFilingEnqueued(ctx context.Context, ev FilingEnqueued) error {
	attempt, err := uc.loadAttempt(ctx, ev.TenantID, ev.FilingAttemptID)
	if err != nil {
		return err
	}
	if attempt == nil || attempt.Status != StatusEnfileirado {
		return nil // já processada (idempotente)
	}

	// ENFILEIRADO → PROTOCOLANDO (guarda status: só um worker ganha).
	if err := uc.repo.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		return uc.rw.MarkFilingProtocolando(ctx, tx, attempt.ID)
	}); err != nil {
		return err
	}
	attempt, err = uc.loadAttempt(ctx, ev.TenantID, ev.FilingAttemptID)
	if err != nil {
		return err
	}
	if attempt.Status != StatusProtocolando {
		return nil // outro worker ganhou — aborta graciosamente
	}

	// Decifra a credencial (em memória) e carrega o PDF congelado.
	var login, password string
	if err := uc.repo.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		l, p, e := openEsajCredential(ctx, tx, uc.rw, uc.vault, ev.TenantID, attempt.RequestedBy)
		if e != nil {
			return e
		}
		login, password = l, p
		return nil
	}); err != nil {
		return uc.fail(ctx, ev, attempt, err)
	}

	pdf, err := uc.storage.GetBytes(ctx, attempt.PdfStorageKey)
	if err != nil {
		return uc.fail(ctx, ev, attempt, apperr.NewInfra("ler pdf do filing", err))
	}

	// RPA e-SAJ. Os metadados de comarca/vara/classe/partes vêm do processo
	// vinculado ao draft; quando indisponíveis (ainda não vinculado), ficam
	// vazios — o adapter é calibrado em staging (área que exige verificação manual).
	var cnj, comarca, vara, classe, partyNames string
	if err := uc.repo.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		d, e := uc.rw.GetDraftDetail(ctx, tx, ev.TenantID, attempt.DraftID)
		if e != nil {
			return e
		}
		if d.Process != nil {
			cnj = d.Process.CNJNumber
			comarca = d.Process.Court
			vara = d.Process.JudgingBody
			classe = d.Process.Class
			for _, p := range d.Parties {
				if partyNames != "" {
					partyNames += "; "
				}
				partyNames += p.Name
			}
		}
		return nil
	}); err != nil {
		slog.WarnContext(ctx, "filing: metadados do processo indisponíveis", "draft_id", attempt.DraftID, "error", err)
	}
	res, err := uc.gateway.Protocol(ctx, FilingRequest{
		TenantID:     ev.TenantID,
		DraftID:      attempt.DraftID,
		PDF:          pdf,
		Login:        login,
		Password:     password,
		CNJ:          cnj,
		Comarca:      comarca,
		Vara:         vara,
		PetitionType: classe,
		PartyNames:   partyNames,
	})
	if err != nil {
		return uc.fail(ctx, ev, attempt, apperr.NewInfra("rpa esaj", err))
	}

	// Upload dos screenshots (fora da tx) antes de finalizar.
	keys := make([]string, 0, len(res.Screenshots))
	for i, shot := range res.Screenshots {
		key := storage.NewKey(ev.TenantID, fmt.Sprintf("pecas/%s/filing/%s-%d.png", attempt.DraftID, attempt.ID, i))
		if err := uc.storage.PutBytes(ctx, key, "image/png", shot); err != nil {
			return uc.fail(ctx, ev, attempt, apperr.NewInfra("salvar screenshot", err))
		}
		keys = append(keys, key)
	}

	// Sucesso: cria a petition + marca PROTOCOLADO + publica eventos (uma tx).
	return uc.repo.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		petition, e := uc.createPetitionForFiling(ctx, tx, ev.TenantID, attempt.DraftID, res.FilingNumber, res.Receipt)
		if e != nil {
			return e
		}
		if _, e := uc.rw.MarkFilingProtocolado(ctx, tx, attempt.ID, petition.ID, res.FilingNumber, keys); e != nil {
			return e
		}
		// Propaga o nº de protocolo automático pra coluna do draft (idempotente:
		// só grava se ainda nulo) — a tela "Concluída" do solicitante lê
		// draft.filing_number, então o número precisa estar aqui também.
		if e := uc.rw.UpdateFilingNumber(ctx, tx, attempt.DraftID, ev.TenantID, res.FilingNumber); e != nil {
			return e
		}
		if uc.outbox != nil {
			if e := uc.outbox.Publish(ctx, tx, newPetitionFiled(petition, ev.TenantID)); e != nil {
				return e
			}
			if e := uc.outbox.Publish(ctx, tx, newFilingSucceeded(attempt.DraftID, ev.TenantID, attempt.ID, petition.ID, res.FilingNumber)); e != nil {
				return e
			}
		}
		return nil
	})
}

// createPetitionForFiling recria a lógica do File (manual) para a petição
// automática: resolve court_record, InsertPetition + MarkFiled + saga FILED.
// Reusa os MESMOS métodos de repo (fonte única de verdade do SQL).
func (uc *FilingUseCase) createPetitionForFiling(ctx context.Context, tx database.Tx, tenantID, draftID, filingNumber string, receipt map[string]any) (*Petition, error) {
	d, err := uc.rw.GetDraftByID(ctx, tx, tenantID, draftID)
	if err != nil {
		return nil, err
	}
	courtRecordID := ""
	if d.IntimationID != "" {
		crid, crErr := uc.rw.GetCourtRecordIDByIntimation(ctx, tx, tenantID, d.IntimationID)
		if crErr != nil {
			return nil, crErr
		}
		courtRecordID = crid
	}
	if courtRecordID == "" {
		return nil, ErrCourtRecordRequired
	}
	petition, err := uc.rw.InsertPetition(ctx, tx, &Petition{
		DraftID:       draftID,
		CourtRecordID: courtRecordID,
		FiledAt:       uc.now(),
		Receipt:       receipt,
	})
	if err != nil {
		return nil, err
	}
	if _, err := uc.rw.UpdateSagaState(ctx, tx, draftID, tenantID, SagaStateFiled, false, "", nil); err != nil {
		return nil, err
	}
	if err := uc.rw.MarkFiled(ctx, tx, draftID, tenantID, filingNumber); err != nil {
		return nil, err
	}
	return petition, nil
}

// fail finaliza a tentativa como FALHOU e publica filing.failed (uma tx).
// Retorna nil para que o listener trate como terminal (ack) — a falha já está
// registrada e o fallback manual permanece disponível (critério 4).
func (uc *FilingUseCase) fail(ctx context.Context, ev FilingEnqueued, attempt *FilingAttempt, cause error) error {
	reason := sanitizeFilingError(cause)
	if markErr := uc.repo.Do(ctx, ev.TenantID, func(tx database.Tx) error {
		if e := uc.rw.MarkFilingFailed(ctx, tx, attempt.ID, reason); e != nil {
			return e
		}
		if uc.outbox != nil {
			if e := uc.outbox.Publish(ctx, tx, newFilingFailed(attempt.DraftID, ev.TenantID, attempt.ID, reason)); e != nil {
				return e
			}
		}
		return nil
	}); markErr != nil {
		slog.ErrorContext(ctx, "filing: falha ao marcar FALHOU (attempt fica PROTOCOLANDO)",
			"filing_attempt_id", attempt.ID, "error", markErr, "original_cause", cause)
	}
	return nil
}

// sanitizeFilingError traduz erros internos do RPA e-SAJ (strings cruas como
// "rpa esaj: esaj login: navigate: ...") em mensagens amigáveis ao advogado que
// vão para o failure_reason da attempt — sem vazar detalhes de implementação.
func sanitizeFilingError(cause error) string {
	if cause == nil {
		return "Erro ao protocolar — tente novamente ou protocole manualmente"
	}
	msg := strings.ToLower(cause.Error())
	switch {
	case strings.Contains(msg, "credencial") || strings.Contains(msg, "senha") || strings.Contains(msg, "login"):
		return "Credencial e-SAJ inválida ou expirada"
	case strings.Contains(msg, "certificado"):
		return "Certificado digital expirado — renove para continuar"
	case strings.Contains(msg, "captcha") || strings.Contains(msg, "anti-bot") || strings.Contains(msg, "verificação"):
		return "Tribunal solicitou verificação manual — protocole manualmente"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "context deadline") || strings.Contains(msg, "tempo limite"):
		return "Tempo limite excedido — tente novamente"
	default:
		return "Erro ao protocolar — tente novamente ou protocole manualmente"
	}
}

func (uc *FilingUseCase) loadAttempt(ctx context.Context, tenantID, id string) (*FilingAttempt, error) {
	var out *FilingAttempt
	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		a, e := uc.rw.GetFilingAttempt(ctx, tx, tenantID, id)
		if e != nil {
			return e
		}
		out = a
		return nil
	})
	return out, err
}

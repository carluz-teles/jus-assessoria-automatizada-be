package deadline

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/lib/apperr"
)

// fakeFailingComposer implements advisory.PromptComposer and makes Compose fail with a fixed
// error, so ClassifyType's error-propagation path can be exercised without depending on
// TemplateComposer's internal "unknown agent" behavior. Only Compose is exercised by
// ClassifyType; the rest are stubs to satisfy the interface.
type fakeFailingComposer struct {
	err error
}

func (f fakeFailingComposer) Compose(string, advisory.CaseContext) (advisory.Composed, error) {
	return advisory.Composed{}, f.err
}
func (fakeFailingComposer) ComposeProcess(string, advisory.ProcessContext) (advisory.Composed, error) {
	return advisory.Composed{}, nil
}
func (fakeFailingComposer) ComposeDraft(string, advisory.DraftContext) (advisory.Composed, error) {
	return advisory.Composed{}, nil
}
func (fakeFailingComposer) ComposeChat(string, advisory.ChatContext) (advisory.Composed, error) {
	return advisory.Composed{}, nil
}
func (fakeFailingComposer) ComposeReview(string, advisory.ReviewContext) (advisory.Composed, error) {
	return advisory.Composed{}, nil
}
func (fakeFailingComposer) ComposeTheses(string, advisory.DraftContext) (advisory.Composed, error) {
	return advisory.Composed{}, nil
}
func (fakeFailingComposer) ComposeIterate(string, advisory.IterateContext) (advisory.Composed, error) {
	return advisory.Composed{}, nil
}

var _ advisory.PromptComposer = fakeFailingComposer{}

// A nil generator (OpenRouter unconfigured) yields ErrClassifierUnavailable — the ingest-time
// fallback degrades gracefully (design §"Fallback IA": "se indisponível, o prazo omisso
// permanece a_apurar sem tipo inferido") instead of erroring the whole ingest.
func TestClassifyType_NilGenerator_Unavailable(t *testing.T) {
	uc := NewClassifyUseCase(advisory.NewTemplateComposer(), nil, "")
	_, err := uc.ClassifyType(context.Background(), "t", advisory.CaseContext{})
	if !errors.Is(err, ErrClassifierUnavailable) {
		t.Errorf("error = %v, want ErrClassifierUnavailable", err)
	}
}

// A composer fault propagates as-is (ClassifyType adds no wrapping of its own).
func TestClassifyType_ComposerError_Propagates(t *testing.T) {
	composerErr := apperr.NewInvalid("advisory: unknown prompt agent bogus")
	uc := NewClassifyUseCase(fakeFailingComposer{err: composerErr}, &fakeGen{}, "")
	_, err := uc.ClassifyType(context.Background(), "t", advisory.CaseContext{})
	if !errors.Is(err, composerErr) {
		t.Errorf("error = %v, want the composer's error %v", err, composerErr)
	}
}

// A generator (LLM) fault propagates as-is.
func TestClassifyType_GeneratorError_Propagates(t *testing.T) {
	genErr := errors.New("openrouter: timeout")
	gen := &fakeGen{err: genErr}
	uc := NewClassifyUseCase(advisory.NewTemplateComposer(), gen, "")
	_, err := uc.ClassifyType(context.Background(), "t", advisory.CaseContext{})
	if !errors.Is(err, genErr) {
		t.Errorf("error = %v, want the generator's error %v", err, genErr)
	}
}

// A malformed JSON answer from the model surfaces a typed infra error (never a panic, never a
// silently-guessed classification).
func TestClassifyType_MalformedJSON_TypedInfraError(t *testing.T) {
	gen := &fakeGen{out: []byte(`{not valid json`)}
	uc := NewClassifyUseCase(advisory.NewTemplateComposer(), gen, "")
	_, err := uc.ClassifyType(context.Background(), "t", advisory.CaseContext{})
	if err == nil {
		t.Fatal("error = nil, want a typed parse error")
	}
	var appErr *apperr.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v (%T), want an *apperr.AppError", err, err)
	}
	if appErr.Kind != apperr.KindInfra {
		t.Errorf("error kind = %q, want %q", appErr.Kind, apperr.KindInfra)
	}
}

// The safety-critical branch (design §"nunca chuta"): a well-formed answer whose `tipo` field is
// empty — the model itself declined to guess — must return ErrClassifierUnavailable, NOT a
// classification with an empty tipo silently accepted downstream.
func TestClassifyType_EmptyTipo_Unavailable(t *testing.T) {
	gen := &fakeGen{out: []byte(`{"tipo":"","confianca":0,"alternativa":""}`)}
	uc := NewClassifyUseCase(advisory.NewTemplateComposer(), gen, "")
	got, err := uc.ClassifyType(context.Background(), "t", advisory.CaseContext{})
	if !errors.Is(err, ErrClassifierUnavailable) {
		t.Errorf("error = %v, want ErrClassifierUnavailable", err)
	}
	if got != (ClassifiedType{}) {
		t.Errorf("result = %+v, want the zero value on an unavailable classification", got)
	}
}

// Happy path: a well-formed answer with tipo/confiança/alternativa populated is returned as-is,
// and the request reaching the generator carries the classify_intimation_type schema + tenant.
func TestClassifyType_HappyPath(t *testing.T) {
	gen := &fakeGen{out: []byte(`{"tipo":"Contestação","confianca":0.82,"alternativa":"Réplica"}`)}
	uc := NewClassifyUseCase(advisory.NewTemplateComposer(), gen, "openai/gpt-4o-mini")

	got, err := uc.ClassifyType(context.Background(), "tenant-1", advisory.CaseContext{IntimationText: "Fica a parte intimada."})
	if err != nil {
		t.Fatalf("ClassifyType() error = %v", err)
	}
	if got.Tipo != "Contestação" || got.Confianca != 0.82 || got.Alternativa != "Réplica" {
		t.Errorf("result = %+v, want {Contestação, 0.82, Réplica}", got)
	}
	if gen.gotReq.SchemaName != "classify_intimation_type" || len(gen.gotReq.Schema) == 0 {
		t.Errorf("request schema not set: name=%q schema_len=%d", gen.gotReq.SchemaName, len(gen.gotReq.Schema))
	}
	if gen.gotReq.TenantID != "tenant-1" {
		t.Errorf("request tenant_id = %q, want tenant-1", gen.gotReq.TenantID)
	}
	if gen.gotReq.UseCase != "deadline.classify_type" {
		t.Errorf("request use_case = %q, want deadline.classify_type", gen.gotReq.UseCase)
	}
}

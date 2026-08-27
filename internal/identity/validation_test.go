package identity

import (
	"testing"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// validAddress is a well-formed address reused across the request tests so each
// case varies only the field under test.
func validAddress() Address {
	return Address{
		CEP:        "01311-902",
		Logradouro: "Av Paulista",
		Numero:     "1000",
		Cidade:     "São Paulo",
		UF:         "SP",
	}
}

func validRequest() UpdateOrgProfileRequest {
	return UpdateOrgProfileRequest{
		CNPJ:      "12.345.678/0001-95",
		LegalName: "Escritório LTDA",
		TradeName: "Escritório",
		Address:   validAddress(),
	}
}

func TestUpdateOrgProfileRequest_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*UpdateOrgProfileRequest)
		wantErr bool
		// field is the json key expected in the validation.Errors map on failure.
		field string
	}{
		{name: "masked cnpj is accepted", mutate: func(*UpdateOrgProfileRequest) {}, wantErr: false},
		{
			name:    "bare 14-digit cnpj is accepted",
			mutate:  func(r *UpdateOrgProfileRequest) { r.CNPJ = "12345678000195" },
			wantErr: false,
		},
		{
			name:    "cnpj with too few digits is accepted (no format check)",
			mutate:  func(r *UpdateOrgProfileRequest) { r.CNPJ = "12.345.678/0001" },
			wantErr: false,
		},
		{
			name:    "cnpj with letters is accepted (no format check)",
			mutate:  func(r *UpdateOrgProfileRequest) { r.CNPJ = "12A45678000195XY" },
			wantErr: false,
		},
		{
			name:    "empty cnpj is required",
			mutate:  func(r *UpdateOrgProfileRequest) { r.CNPJ = "" },
			wantErr: true,
			field:   "cnpj",
		},
		{
			name:    "missing legal_name is rejected",
			mutate:  func(r *UpdateOrgProfileRequest) { r.LegalName = "" },
			wantErr: true,
			field:   "legal_name",
		},
		{
			name:    "missing trade_name is rejected",
			mutate:  func(r *UpdateOrgProfileRequest) { r.TradeName = "" },
			wantErr: true,
			field:   "trade_name",
		},
		{
			name:    "address without cep is accepted (street optional)",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Address.CEP = "" },
			wantErr: false,
		},
		{
			name:    "cidade + uf only is accepted (street optional)",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Address = Address{Cidade: "Franca", UF: "SP"} },
			wantErr: false,
		},
		{
			name:    "address missing uf is rejected",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Address.UF = "" },
			wantErr: true,
			field:   "address",
		},
		{
			name:    "empty address is valid (optional as a whole)",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Address = Address{} },
			wantErr: false,
		},
		{
			name:    "address missing cidade is rejected",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Address = Address{UF: "SP"} },
			wantErr: true,
			field:   "address",
		},
		{
			name:    "empty email is valid (optional)",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Email = "" },
			wantErr: false,
		},
		{
			name:    "well-formed email is accepted",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Email = "contato@escritorio.com.br" },
			wantErr: false,
		},
		{
			name:    ".com.br email is accepted",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Email = "advogado@escritorio.com.br" },
			wantErr: false,
		},
		{
			name:    ".adv.br email is accepted",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Email = "dr@banca.adv.br" },
			wantErr: false,
		},
		{
			name:    "malformed email is rejected",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Email = "not-an-email" },
			wantErr: true,
			field:   "email",
		},
		{
			name:    "email missing @ is rejected",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Email = "foo.com.br" },
			wantErr: true,
			field:   "email",
		},
		{
			name:    "empty phone is valid (optional)",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Phone = "" },
			wantErr: false,
		},
		{
			name:    "10-digit phone is accepted",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Phone = "1133334444" },
			wantErr: false,
		},
		{
			name:    "11-digit phone is accepted",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Phone = "11987654321" },
			wantErr: false,
		},
		{
			name:    "9-digit phone is rejected",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Phone = "119876543" },
			wantErr: true,
			field:   "phone",
		},
		{
			name:    "phone with letters is rejected",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Phone = "11ABCDE4321" },
			wantErr: true,
			field:   "phone",
		},
		{
			name:    "masked phone is rejected (bare digits only)",
			mutate:  func(r *UpdateOrgProfileRequest) { r.Phone = "(11) 98765-4321" },
			wantErr: true,
			field:   "phone",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := validRequest()
			tt.mutate(&req)

			err := req.Validate()
			if tt.wantErr == (err == nil) {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			verrs, ok := err.(validation.Errors)
			if !ok {
				t.Fatalf("error type = %T, want validation.Errors", err)
			}
			if _, has := verrs[tt.field]; !has {
				t.Fatalf("validation.Errors = %v, want a %q entry", verrs, tt.field)
			}
		})
	}
}

func TestToOrgProfile_NormalizesCNPJ(t *testing.T) {
	req := validRequest() // CNPJ carries the mask "12.345.678/0001-95"
	req.Phone = "11987654321"
	req.Email = "contato@escritorio.com.br"

	got := req.toOrgProfile()
	if got.CNPJ != "12345678000195" {
		t.Fatalf("CNPJ = %q, want the bare digits", got.CNPJ)
	}
	if got.LegalName != req.LegalName || got.TradeName != req.TradeName || got.Address != req.Address {
		t.Fatalf("toOrgProfile() dropped a field: %+v", got)
	}
	// Phone and email pass through unchanged (no mask stripping like CNPJ).
	if got.Phone != req.Phone {
		t.Fatalf("Phone = %q, want %q passed through", got.Phone, req.Phone)
	}
	if got.Email != req.Email {
		t.Fatalf("Email = %q, want %q passed through", got.Email, req.Email)
	}
}

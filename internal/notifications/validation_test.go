package notifications

import "testing"

// AC: ValidNotificationType accepts every Type*Aviso this slice knows about
// (both the in-app record() types and the generic-EMAIL member_joined type) and
// rejects anything else, including the empty string.
func TestValidNotificationType(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		want bool
	}{
		{name: "import_finished", typ: TypeImportFinished, want: true},
		{name: "new_andamento", typ: TypeNewAndamento, want: true},
		{name: "deadline_due_soon", typ: TypeDeadlineDueSoonAviso, want: true},
		{name: "deadline_missed", typ: TypeDeadlineMissedAviso, want: true},
		{name: "trial_ending_soon", typ: TypeTrialEndingSoonAviso, want: true},
		{name: "payment_failed", typ: TypePaymentFailedAviso, want: true},
		{name: "member_joined", typ: TypeMemberJoinedAviso, want: true},
		{name: "unknown", typ: "totally_made_up", want: false},
		{name: "empty", typ: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidNotificationType(tt.typ); got != tt.want {
				t.Errorf("ValidNotificationType(%q) = %v, want %v", tt.typ, got, tt.want)
			}
		})
	}
}

// AC: SetPreferenceRequest.Validate rejects an unknown type as a 400-shaped error
// (KindInvalid), same boundary as an unknown channel.
func TestSetPreferenceRequest_Validate_UnknownType(t *testing.T) {
	req := SetPreferenceRequest{Type: "totally_made_up", Channels: []string{ChannelEmail}}
	if err := req.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for an unknown type")
	}
}

// AC: a known type with a valid channel set passes validation.
func TestSetPreferenceRequest_Validate_KnownType(t *testing.T) {
	req := SetPreferenceRequest{Type: TypeMemberJoinedAviso, Channels: []string{ChannelEmail}}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

package acquisition

import "testing"

func mv(codes ...int) []datajudMovimento {
	out := make([]datajudMovimento, 0, len(codes))
	for _, c := range codes {
		out = append(out, datajudMovimento{Codigo: c})
	}
	return out
}

func TestFaseFromClassAndMovimentos(t *testing.T) {
	tests := []struct {
		name   string
		classe string
		movs   []datajudMovimento
		want   Fase
	}{
		{"conhecimento por padrão", "Procedimento Comum Cível", mv(26, 85, 60), FaseConhecimento},
		{"saneamento → instrução", "Procedimento Comum", mv(26, 12387), FaseInstrucao},
		{"julgamento → sentença", "Procedimento Comum", mv(26, 970, 193), FaseSentenca},
		{"recurso avança sobre sentença", "Procedimento Comum", mv(193, 804), FaseRecurso},
		{"cumprimento iniciado → execução", "Procedimento Comum", mv(193, 11385), FaseExecucao},
		{"classe de execução já começa em execução", "Execução de Título Extrajudicial", mv(26, 85), FaseExecucao},
		{"cumprimento de sentença baseline execução", "Cumprimento de Sentença", mv(60), FaseExecucao},
		{"pega a fase MAIS AVANÇADA, não a última", "Procedimento Comum", mv(11385, 970), FaseExecucao},
		{"movimentos neutros não movem a fase", "Procedimento Comum", mv(60, 581, 92, 123), FaseConhecimento},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := faseFromClassAndMovimentos(tt.classe, tt.movs); got != tt.want {
				t.Errorf("faseFromClassAndMovimentos(%q, %v) = %q, want %q", tt.classe, tt.movs, got, tt.want)
			}
		})
	}
}

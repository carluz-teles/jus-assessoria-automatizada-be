# jus-assessoria — Arquitetura da IA (advisory)

> **Status:** desenho (v0). Nenhuma linha de IA existe ainda — `worker-ai` é esqueleto,
> não há slice de `document`/`draft`/`review`, nem cliente LLM. Este doc é o mapa: como
> orquestrar agentes especializados sobre o acervo já capturado (acquisition), com output
> estruturado, validação e feedback loop, para produzir **assessoria** confiável.
> **Fonte de verdade do schema:** `erd-modelo-de-dados.md`. Onde divergir, o schema vence.

## 0. O ponto de partida — o schema já foi desenhado pra isso

O modelo de dados antecipa a filosofia inteira. Não estamos inventando; estamos preenchendo:

| Tabela / coluna | O que já codifica |
|---|---|
| `document` (COURT\|UPLOAD, `has_text_layer`, `extractor_version`) | ingestão + decisão de OCR + versão do extrator |
| `chunk` (`text`, `embedding vector(1536)`) | **RAG** — recuperação por similaridade (pgvector) |
| `draft` (`piece_type`, `saga_state` CREATED→EXTRACTING→REVIEWED→SIGNED→FILED→LABELED) | a **saga** do ciclo advisory |
| `review.findings jsonb` — *"Finding[] with citations"* | **output estruturado + citação obrigatória** (grounding) |
| `review.coverage jsonb` — *"{verified[], notVerified[]}"* | **passo de validação**: o que foi comprovado vs não |
| `review.model_version` + `rules_version` | **versionamento** de prompt/modelo/regra (reprodutibilidade, A/B) |
| `petition.observed_result` (OK\|AMENDMENT\|NOT_ADMITTED\|UNTIMELY), índice *"close the loop"* | **feedback loop** de resultado real |
| `deadline` (BUSINESS\|CALENDAR, `doubled`, `holidays_applied` auditável) | prazos determinísticos (não é trabalho de LLM) |

**Leitura:** o `review` é o coração — findings com citação, coverage de verificação, e versão.
Toda a defesa anti-alucinação está embutida na forma da tabela. Falta o código que a preenche.

## 1. Princípios (decididos)

1. **Grounding obrigatório.** Todo claim de análise cita um `chunk`/`document` (ou um `docket_entry`
   real). Claim sem lastro não vira fato — vai pra `coverage.notVerified[]` e é sinalizado, nunca exibido
   como certeza. **Sem documento ingerido e chunked, não há o que citar** → ingestão é o pré-requisito nº1.
2. **Output estruturado, não prosa.** Cada agente devolve JSON validado por schema (`output_config.format`
   na Messages API, ou `strict: true` em tool use). Elimina parse frágil e reduz variância.
3. **Validação como passo separado.** Um validador (modelo barato ou determinístico) confere cada finding
   contra sua citação antes de persistir. É o portão que popula `review.coverage`.
4. **Feedback loop fecha o ciclo.** `petition.observed_result` volta e correlaciona com
   `model_version`/`rules_version` que geraram o draft → a camada meta melhora prompt/playbook com o desfecho.
5. **Meta-prompting, não prompt monolítico.** As instruções de cada agente são **compostas por caso**
   (tribunal, rito, tipo de peça, fase, playbook do escritório), não um prompt fixo gigante.
6. **Advisory + human-in-the-loop.** A saída é sempre `draft`/sugestão; o advogado aprova, refina ou
   descarta. Nunca protocola sozinho. (É o "Aprovar tudo / Refinar / Criar tarefa" das refs de UX.)
7. **Determinístico onde dá.** Prazos, contagem de dias úteis, feriados, dedup por hash — **código**, não
   LLM. LLM só onde há linguagem/julgamento.

## 2. Workflow orquestrado, não "agentes autônomos"

Decisão de superfície (skill `claude-api` → *Should I Build an Agent?*): nosso pipeline é **workflow
code-orchestrated** — nós controlamos o loop, cada etapa é uma chamada única ao Claude com input/output
bem definidos. **Não** é um agente aberto que decide a própria trajetória, e **não** é Managed Agents
(que faz sentido quando a Anthropic hospeda o loop + container; aqui a orquestração é nossa, no
`worker-ai`, transacional com outbox/saga).

- **Cada "agente especializado" = um estágio do workflow** com um prompt estreito + schema de saída.
- O loop, a saga (`draft.saga_state`), a idempotência e o outbox ficam no nosso worker (Go + `anthropic-sdk-go`).
- Reservamos "agente de verdade" (tool use com loop) só para tarefas genuinamente exploratórias (ex.: um
  agente que decide *quais* documentos buscar). No v0, o retrieval é determinístico.

## 3. Meta-prompting — por que ajuda MUITO no nosso cenário

**Problema:** heterogeneidade brutal do jurídico BR. A mesma intimação exige ações diferentes por tribunal
(TJSP≠TRT≠STJ), rito (cível/trabalhista/execução/JEC), tipo de peça, fase, **regime de prazo** (CPC dias
úteis, CLT, dobrado p/ litisconsortes/fazenda) e — decisivo — o **playbook do escritório**. Um prompt
estático único é raso ou vira um monstro que dá drift.

**Meta-prompting = compor o instruction-set por caso**, a partir de:
- **contexto do banco** (court/degree/class/subject/piece_type/fase, do `court_record`+`docket_entry`);
- **playbook do escritório** (derivado dos `draft`/`petition`/`observed_result` passados dele — a "voz" do
  escritório, o "com base no histórico" das refs de UX);
- **camada de regras** (`rules_version` — regimes de prazo, quirks de tribunal).

Ganhos concretos aqui:
1. **Precisão sem monolito** → instruções enxutas por agente = menos ruído, menos alucinação, mais consistência.
2. **A voz do escritório** → a minuta sai no estilo/precedentes daquele escritório (diferencial de produto).
3. **Versionável e testável** → `rules_version`/`model_version` correlacionam com `observed_result` → os
   prompts **melhoram com o desfecho real**. Esse é o valor composto do meta-prompting.
4. **Tiering de custo** → a camada meta roteia modelo por dificuldade (ver §7).

O "meta-prompt" (o compositor) é ele mesmo um artefato versionado. Numa fatia futura, pode ser um passo LLM
que *gera/refina* os prompts dos sub-agentes contra um golden set + outcomes — mas v0 começa com composição
determinística (templates + contexto), que já entrega 80%.

## 4. Os agentes especializados (o fan-out)

```
0. CONTEXTO (determinístico — SQL, NÃO é LLM):
   court_record + docket_entry + document→chunk (RAG top-k) + deadline + playbook do escritório
        │  a camada meta especializa as instruções de cada agente com esse contexto
        ▼
1. FAN-OUT (cada agente: prompt estreito + schema de saída estruturado)
   ├─ Análise de documento  → fatos/estrutura do despacho + docs citados         → findings[]
   ├─ Risco                 → prazos, preclusão, chance de êxito, red flags        → risks[]
   ├─ Extração de tarefas    → ações acionáveis + prazo sugerido (o "Criar tarefa") → tasks[]
   └─ Geração de minuta      → a peça, na voz do escritório                        → draft (storage_key)
        ▼
2. VALIDAÇÃO (o design coverage/verified)
   cada finding carrega citação → validador confere lastro → review.coverage.{verified,notVerified}
        ▼
3. PERSISTÊNCIA (tx + outbox)  review(findings, coverage, model_version, rules_version) + eventos
        ▼
4. FEEDBACK  petition.observed_result → correlaciona com a versão → atualiza playbook/prompts
```

Cada agente tem **uma responsabilidade** e um **schema de saída** — exemplo do agente de análise
(esboço; o schema real vive no slice):

```jsonc
// output_config.format do agente de análise de intimação
{
  "summary": "string",                    // "o que aconteceu"
  "findings": [{
    "claim": "string",                    // afirmação factual
    "citation": { "document_id": "uuid", "chunk_id": "uuid", "quote": "string" },
    "kind": "OBLIGATION | DEADLINE | RISK | INFO"
  }],
  "suggested_deadline": { "days": 10, "counting": "BUSINESS", "anchor": "docket_entry_id" }
}
```

## 5. Grounding e validação — o gate anti-alucinação

**Como citar (constraint real da API):** as **citations nativas** da Messages API
(`citations: {enabled:true}` no bloco `document`) devolvem `cited_text` + localização — perfeitas pra
grounding — **mas são incompatíveis com `output_config.format`** (structured output) na mesma chamada.
Então:
- **Opção A (v0, recomendada):** o próprio schema estruturado carrega a citação como campo
  (`citation.chunk_id` + `quote`), e um **validador determinístico** confere que a `quote` existe de fato
  naquele `chunk.text` (substring/normalização). Barato, sem 2º LLM, e é o que popula `coverage`.
- **Opção B (quando precisar de citação de trecho fina):** uma chamada separada com citations nativas
  sobre o documento, fora do caminho estruturado.

**O validador** (por finding): a `quote` casa com o `chunk`? o `document_id`/`chunk_id` pertence ao
`court_record` do caso (isolamento de tenant)? Se sim → `coverage.verified[]`; se não → `notVerified[]` +
o finding é rebaixado (mostrado como "não confirmado" ou descartado). Isso é o que impede alucinação de
virar fato na tela.

## 6. Feedback loop

O `petition.observed_result` (OK\|AMENDMENT\|NOT_ADMITTED\|UNTIMELY) é o sinal de realidade: a peça que
geramos foi aceita? O índice `petition (court_record_id) WHERE observed_result IS NULL` é literalmente
"close the loop" — varre peças protocoladas aguardando desfecho. Quando o desfecho chega, correlaciona com
o `model_version`/`rules_version` do `draft` que a originou → alimenta métricas por versão de prompt e, na
fatia de meta-prompting, retrolimenta o compositor/playbook.

## 7. Modelos e custo (tiering)

Default `claude-opus-4-8` (adaptive thinking, `effort` por tarefa). Tiering pela dificuldade:

| Etapa | Modelo sugerido | Por quê |
|---|---|---|
| Orquestração / raciocínio de risco / **minuta** | `claude-opus-4-8` (`effort: high`) | julgamento jurídico, voz do escritório |
| Extração / classificação / **validação** de findings | `claude-haiku-4-5` | barato, alto volume, tarefa estreita |
| (futuro) advisor/meta-crítica | opus como advisor tool (beta) | camada meta acima do executor |

**Prompt caching** é essencial na escala (~6,5k processos/tenant): o **playbook do escritório + regras**
(prefixo estável) vão no início do prompt com `cache_control` → cache read (~0,1×) em vez de reprocessar a
cada caso. O contexto volátil do caso vai **depois** do último breakpoint. Isso derruba o custo por análise
drasticamente.

## 8. Ordem de fatias (o caminho de implementação)

Cada fatia = slice pequeno, verde, com pm-plan → dev-qa → code-review. Dependência estrita:

1. **Ingestão de documentos** (`internal/document`): baixar o inteiro teor (S3/R2 presigned), OCR quando
   `has_text_layer=false`, `extractor_version`. **Gargalo nº1 — sem isso os agentes não têm o que citar.**
2. **Chunking + embeddings** (`chunk`, índice pgvector): fatiar + embed + índice de similaridade.
3. **Cliente LLM em `/lib`** (anthropic-sdk-go): wrapper com structured output, caching, retry, tokens/otel.
   Segredo por env (`ANTHROPIC_API_KEY`), nunca no código.
4. **Agente de análise + validação** (`internal/review` produtor): consome um evento (ex.:
   `court_record_observed`/`document_ready`), roda análise → validação → grava `review` (findings+coverage).
5. **Extração de tarefas + prazos** (liga no `deadline` determinístico existente).
6. **Geração de minuta** (`internal/draft`): saga `draft.saga_state`; produz `draft` (storage_key).
7. **Feedback loop**: captura `petition.observed_result` e as métricas por versão.
8. **Meta-prompting real** (opcional/depois): o compositor vira passo LLM versionado com golden set.

## 9. Pontos de falha, gaps e riscos (o que projetar contra)

**Gaps de sistema (pré-requisitos):**
- Sem ingestão/OCR → `chunk`/`embedding` vazios → sem grounding. (fatia 1–2)
- Sem índice pgvector no `chunk.embedding` → sem retrieval.
- Sem representação do **playbook do escritório** → o "histórico" precisa de um caminho de dado derivado
  dos `draft`/`petition`/`observed_result`.
- Sem cliente LLM nem plumbing de structured output.

**Modos de falha da IA → mecanismo que ataca cada um:**
| Falha | Mitigação |
|---|---|
| Alucinação / claim sem lastro | citação obrigatória no schema + **gate de coverage** (validador) |
| Inconsistência entre runs | `output_config.format` (schema) + `effort` calibrado + prompt versionado |
| Estouro de contexto (processo com milhares de andamentos/docs) | **RAG top-k + sumarização em camadas**, não despejar tudo |
| Custo/latência na escala | tiering de modelo + **prompt caching** do playbook/regras |
| Drift silencioso | versionar tudo (`model_version`/`rules_version`) + eval contra golden set + correlação com `observed_result` |
| Risco jurídico de minuta errada | **advisory/DRAFT + aprovação humana**, nunca auto-protocolo |
| Vazamento entre tenants no retrieval | filtro por `tenant_id` (barreira 1) + RLS (barreira 2); citação tem que pertencer ao `court_record` do caso |

## 10. Mapa: agente → tabela

| Produz | Persiste em | Evento |
|---|---|---|
| Análise + validação | `review` (findings, coverage, model_version, rules_version) | `review.created` |
| Tarefas/prazos | `deadline` (determinístico) | `deadline.opened` |
| Minuta | `draft` (saga_state, storage_key) | `draft.created` → `draft.reviewed` … |
| Peça protocolada | `petition` (observed_result) | `petition.filed` → (feedback) |

---

**Resumo de uma linha:** o schema já quer agentes especializados com output estruturado, citação
obrigatória, validação de cobertura e feedback de desfecho. A camada de meta-prompting compõe as
instruções por caso (tribunal/rito/peça/playbook) — é o que torna a assessoria precisa, na voz do
escritório, e que melhora com o resultado real. Começa pela **ingestão de documentos** (sem ela, nada
tem lastro).

-- 0090: action_item — persistir title/description da providência (docs/erd-costura-
-- providencia-tarefa-peca.md). A IA (POST /v1/intimacoes/:id/analise) gera title +
-- description por providência, mas até agora eles viviam só no store efêmero da análise
-- (AnaliseProvidenciaView) e NÃO viajavam no evento acquisition.intimation.analyzed —
-- logo o action_item materializado não os guardava e a UI dependia de cache efêmero.
-- Estas duas colunas persistem o texto exibido no card "Providências" do detalhe da
-- intimação. Nullable, sem default: itens antigos (pré-0090) ficam NULL e a UI mostra "—".
ALTER TABLE action_item
    ADD COLUMN title       text,
    ADD COLUMN description text;

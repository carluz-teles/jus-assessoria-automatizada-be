#!/usr/bin/env python3
"""Gera o seed SQL dos feriados ESTADUAIS para a tabela `holiday` (lib/calendar).

Fonte primária: vacanza/holidays (derivada de lei, cobre as 27 UFs por subdiv).
Cross-check opcional: workalendar — se instalado, imprime as DIVERGÊNCIAS entre as
duas libs no STDERR pra revisão humana (priorize SP/RJ/MG). O cross-check é
guardado por try/except: se a API do workalendar mudar, o core (vacanza) segue.

Feriados NACIONAIS são EXCLUÍDOS: eles são semeados em runtime a partir da
BrasilAPI (lib/calendar.SeedNational), então uma linha STATE nunca duplica uma
data nacional (evita poluir o holidays_applied do deadline).

O SQL vai pro STDOUT; o relatório de divergências vai pro STDERR — então dá pra
redirecionar só o SQL pro arquivo de migration.

Uso (recomendado, sem sujar seu ambiente):
    uv run --with holidays --with workalendar scripts/gen_state_holidays.py 2026 2028 \
        > migrations/0013_holiday_state_seed.up.sql

Ou com pip:
    pip install holidays workalendar
    python3 scripts/gen_state_holidays.py 2026 2028 > migrations/0013_holiday_state_seed.up.sql

Argumentos: <ano_inicial> <ano_final> (inclusive). Sem argumentos usa o ano
corrente + 2.
"""

import sys
from datetime import date

import holidays  # vacanza/holidays

UFS = [
    "AC", "AL", "AP", "AM", "BA", "CE", "DF", "ES", "GO", "MA", "MT", "MS",
    "MG", "PA", "PB", "PR", "PE", "PI", "RJ", "RN", "RS", "RO", "RR", "SC",
    "SP", "SE", "TO",
]


def sql_escape(text: str) -> str:
    """Escapa aspa simples pra literal SQL."""
    return text.replace("'", "''")


def national_dates(years: list[int]) -> set[date]:
    """Datas dos feriados nacionais no período (pra subtrair dos estaduais)."""
    return set(holidays.Brazil(years=years).keys())


def state_holidays(uf: str, years: list[int], nat: set[date]) -> dict[date, str]:
    """Feriados estaduais de uma UF (categoria PUBLIC), sem os nacionais."""
    combined = holidays.Brazil(subdiv=uf, years=years)  # nacional + estadual
    return {d: name for d, name in combined.items() if d not in nat}


def workalendar_state(uf: str, years: list[int], nat: set[date]) -> set[date] | None:
    """Datas de feriado estadual segundo o workalendar (sem nacionais). Retorna
    None quando o workalendar não cobre a UF (aí não há como corroborar)."""
    try:
        from workalendar.registry import registry
    except Exception:
        return None

    cal_cls = registry.get(f"BR-{uf}")
    if cal_cls is None:
        return None
    try:
        cal = cal_cls()
        wk: set[date] = set()
        for year in years:
            wk |= {d for d, _ in cal.holidays(year)}
        return {d for d in wk if d not in nat}
    except Exception as exc:  # noqa: BLE001 — cross-check é best-effort
        print(f"# aviso: workalendar falhou p/ {uf}: {exc}", file=sys.stderr)
        return None


def main() -> int:
    if len(sys.argv) == 3:
        y0, y1 = int(sys.argv[1]), int(sys.argv[2])
    else:
        y0 = date.today().year
        y1 = y0 + 2
    years = list(range(y0, y1 + 1))

    nat = national_dates(years)

    # Viés SEGURO para prazo: só semear datas CORROBORADAS pelas duas libs
    # (interseção). Marcar um feriado falso alongaria o prazo → risco de perda; já
    # omitir um real só o encurta (conservador). Onde o workalendar não cobre a UF,
    # cai pra vacanza (sinalizado). As divergências vão pro STDERR p/ revisão.
    rows: list[tuple[str, date, str]] = []
    fallback_ufs: list[str] = []
    for uf in UFS:
        van = state_holidays(uf, years, nat)
        wk = workalendar_state(uf, years, nat)

        if wk is None:
            chosen = van
            fallback_ufs.append(uf)
        else:
            chosen = {d: name for d, name in van.items() if d in wk}
            only_van = sorted(set(van) - wk)
            only_wk = sorted(wk - set(van))
            if only_van or only_wk:
                print(f"# REVISAR {uf}:", file=sys.stderr)
                for d in only_van:
                    print(f"#   só vacanza (excluído): {d} {van[d]}", file=sys.stderr)
                for d in only_wk:
                    print(f"#   só workalendar (excluído): {d}", file=sys.stderr)

        for d, name in sorted(chosen.items()):
            rows.append((uf, d, name))

    print(f"-- gerado por scripts/gen_state_holidays.py — NÃO editar à mão")
    print(f"-- fonte: vacanza/holidays v{holidays.__version__} ∩ workalendar; anos {y0}-{y1}; UFs: {len(UFS)}")
    print(f"-- viés seguro p/ prazo: só datas corroboradas pelas 2 libs (interseção)")
    print(f"-- feriados nacionais excluídos (semeados em runtime via BrasilAPI)")
    if fallback_ufs:
        print(f"-- UFs sem cobertura workalendar (fallback vacanza, revisar): {', '.join(fallback_ufs)}")
    print()
    if not rows:
        print("-- (nenhum feriado estadual encontrado)")
        return 0
    print("INSERT INTO holiday (scope, scope_id, date, name) VALUES")
    values = [
        f"  ('STATE', '{uf}', '{d.isoformat()}', '{sql_escape(name)}')"
        for uf, d, name in rows
    ]
    print(",\n".join(values))
    print("ON CONFLICT (scope, coalesce(scope_id, ''), date) DO NOTHING;")

    print(f"# {len(rows)} feriados estaduais gerados p/ {len(UFS)} UFs", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

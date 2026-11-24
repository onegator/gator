# Faza 3: Brain

**Cel:** jedno miejsce, w którym zespół i agenci trzymają to, co wiedzą: o produkcie, o tym, jak
budujemy, o ludziach (kto co umie, nad czym pracuje, jak woli pracować) i o decyzjach. Agenci czytają
i proponują przez jeden interfejs (MCP), a ludzie zatwierdzają. Każdy wpis wie, skąd pochodzi i gdzie
go użyto.

**Poza zakresem:** zewnętrzne bazy wektorowe, graf wiedzy jako osobna usługa, trenowanie modeli.

## Co już jest i co z tym robimy

| Dziś | Co się z tym dzieje |
|---|---|
| `product_context` (vision, principles, persona, glossary, decision, lesson, roadmap; proposed→approved) | przenosimy do `brain_entries` (scope `project`), a stary endpoint `/projects/{id}/product` zostaje fasadą przez jedno wydanie |
| `knowledge_entries` (language, architecture, requirements, testing, security) | do `brain_entries` (scope `workspace` lub `project`, kind `convention`) |
| `knowledge_packs` z rejestru | zostają paczkami (faza 4 je dystrybuuje), ich pliki indeksujemy jako wpisy `source.kind='pack'`, tylko do odczytu |
| katalog komponentów + `component_proposals` | zostaje osobno (to struktura, nie wiedza); wpisy Brainu linkują się do komponentów |
| `working_state`, `gator-digest` | zostają per task; digest staje się *źródłem* propozycji Brainu |
| curator (`curation.yaml`, `product/condense.go`) | rozszerzony o rozmowy i pluginy |

## Zakresy i prywatność

| Scope | Kto widzi | Przykłady |
|---|---|---|
| `workspace` | wszyscy | konwencje kodu, standardy security, glosariusz firmy |
| `project` | członkowie projektu | wizja, persony, decyzje, roadmapa, runbooki |
| `component` | członkowie projektu; trafia do jobów dotyczących komponentu i sąsiadów | „auth używa PASETO, nie JWT, bo…" |
| `user` | **domyślnie tylko właściciel**; wpis może mieć `visibility = project/workspace` | ekspertyza, bieżący fokus, preferencje („krótkie PR-y”, „nie pinguj po 18”) |
| `agent` | członkowie projektu | lekcje agenta („w tym repo testy e2e wymagają `make db-up`”) |

**Reguła kontekstu użytkownika dla agenta:** agent dostaje wpisy `user` udostępnione
projektowi/workspace'owi oraz **prywatne wpisy tylko osoby `on_behalf_of`** turnu, czyli tej, która
go wywołała (faza 2: `agent_turns.on_behalf_of`). Prywatne wpisy jednej osoby nigdy nie trafiają do
turnu wywołanego przez kogoś innego. Ten sam warunek obowiązuje w MCP i w pakiecie promptu, a testy
to sprawdzają.

## Dane (`00042_brain.sql`)

```sql
CREATE TABLE brain_entries (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scope         text NOT NULL CHECK (scope IN ('workspace','project','component','user','agent')),
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE,
    component_key text,
    user_id       uuid REFERENCES users(id) ON DELETE CASCADE,
    agent_id      uuid REFERENCES agents(id) ON DELETE CASCADE,
    kind          text NOT NULL CHECK (kind IN (
                    'vision','principle','persona','glossary','decision','lesson','roadmap',
                    'convention','runbook','fact','expertise','focus','preference')),
    title         text NOT NULL,
    content       text NOT NULL,
    visibility    text NOT NULL DEFAULT 'scope' CHECK (visibility IN ('private','scope','project','workspace')),
    status        text NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed','approved','rejected','superseded','archived')),
    version       integer NOT NULL DEFAULT 1,
    supersedes    uuid REFERENCES brain_entries(id),   -- poprzednia wersja / unieważniona decyzja
    valid_from    timestamptz NOT NULL DEFAULT now(),
    valid_until   timestamptz,                          -- ustawiane przy superseded
    confidence    text NOT NULL DEFAULT 'stated' CHECK (confidence IN ('stated','inferred')),
    source        jsonb NOT NULL,   -- {kind: message|task|job|plugin|manual|import|pack, id, url, excerpt}
    created_by_kind text NOT NULL, created_by uuid,
    approved_by   uuid REFERENCES users(id), approved_at timestamptz,
    tsv           tsvector GENERATED ALWAYS AS (to_tsvector('simple', title || ' ' || content)) STORED,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX brain_tsv_idx ON brain_entries USING gin (tsv);
CREATE INDEX brain_scope_idx ON brain_entries (project_id, scope, kind, status);

CREATE TABLE brain_links (            -- wpis ↔ wpis | komponent | task | conversation
    entry_id   uuid NOT NULL REFERENCES brain_entries(id) ON DELETE CASCADE,
    target_kind text NOT NULL, target_id text NOT NULL,
    relation   text NOT NULL DEFAULT 'relates' CHECK (relation IN ('relates','depends_on','contradicts','implements')),
    PRIMARY KEY (entry_id, target_kind, target_id)
);

CREATE TABLE brain_citations (        -- które wpisy dostał / zacytował job
    job_id   uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    entry_id uuid NOT NULL REFERENCES brain_entries(id) ON DELETE CASCADE,
    how      text NOT NULL CHECK (how IN ('packed','searched','cited')),
    PRIMARY KEY (job_id, entry_id, how)
);
```

`00043_brain_backfill.sql` przenosi `product_context` i `knowledge_entries`, zachowując statusy,
autorów i `source_task_id`. Stare tabele zostają do następnego wydania (krok down przywraca dane).

**Wyszukiwanie:** zaczynamy od Postgres FTS (`tsvector` + `ts_rank`) z filtrami scope/kind, co nie
wymaga żadnej nowej zależności. `00044_brain_embeddings.sql` jest opcjonalna: rozszerzenie
`vector` (pgvector), kolumna `embedding vector(1024)` i wyszukiwanie hybrydowe (FTS + kosinus, RRF).
Wymaga obrazu Postgresa z pgvector (`pgvector/pgvector:pg16` w `docker-compose.yml`, `deploy/compose.yaml`,
`install.sh`) i dostawcy embeddingów za interfejsem `brain.Embedder` (np. Voyage albo lokalny model
przez Ollama). Wchodzi dopiero, gdy pomiary pokażą, że FTS nie wystarcza.

## Skąd Brain się uczy

| Źródło | Mechanizm | Wynik |
|---|---|---|
| ręcznie | ekran Brain, `/brain propose` w composerze | `proposed` lub od razu `approved`, jeśli pisze admin projektu |
| zakończony job | `gator-digest` (`decided`, `rejected`), `gator-cli product propose` | propozycje `decision` / `lesson` z linkiem do joba |
| rozmowa | curator: po 30 min ciszy w wątku z ≥ N wiadomościami albo przy zamknięciu taska wyciąga „co ustaliliśmy" | propozycje z `source.kind='message'` i cytatem; karta `brain_proposal` w wątku |
| pluginy | nowa core method `brain.propose` (np. ADR-y z repo przez GitHub, dokumenty Linear, powtarzające się błędy z Sentry jako `runbook`) | propozycje `source.kind='plugin'` |
| ludzie (kontekst użytkownika) | ekran „Ja" (ekspertyza, preferencje); `focus` wyliczany automatycznie z przypisanych tasków i aktywnych wątków | `focus` ma `confidence='inferred'` i nie wymaga zatwierdzenia, ale jest prywatny |

**Sprzeczności:** propozycja, która jest podobna (FTS/embedding) do zatwierdzonej decyzji o innej
treści, dostaje link `contradicts` i w karcie pytanie „to zastępuje decyzję X?". Akceptacja ustawia
`superseded` na starej, z `valid_until`. Historia zostaje, a pytanie „co wiedzieliśmy w marcu" ma
odpowiedź.

## Jak agent korzysta z Brainu

1. **Pakiet** (deterministyczny, jak dziś product slice): zatwierdzone wpisy `component` dla
   komponentu taska i sąsiadów, `principle`/`vision` projektu, `convention` workspace'u, a do tego
   kontekst `user` według reguły prywatności. Wszystko w budżecie bajtów, zapisane jako
   `brain_citations(how='packed')`.
2. **Wyszukiwanie w trakcie pracy** przez serwer MCP (niżej): `how='searched'`.
3. **Cytowanie:** prompt prosi o `[brain:<id>]` przy korzystaniu z wpisu; runner wyciąga to z
   odpowiedzi (`how='cited'`), a UI pokazuje „skąd to wie". Wpisy nigdy nie cytowane przez 90 dni
   trafiają do przeglądu („czy to jeszcze prawda?").

## Serwer MCP `gator-brain`

Endpoint w gator-server: `POST /mcp` (Streamable HTTP), autoryzacja Bearer:
- **token joba** (`gtr_job_…`): zakres projektu taska i reguła `on_behalf_of`; runner dopisuje
  ten serwer do `--mcp-config` (dziś jest „explicit or empty", więc wpis Gatora jest zawsze obecny);
- **token użytkownika**: człowiek podpina Brain do własnego Claude Code, Cursora czy Codexa
  (`claude mcp add --transport http gator https://…/mcp --header "Authorization: Bearer gtr_user_…"`).
  Widzi to, co widzi w aplikacji.

Narzędzia:

```
brain_search(query, scope?, kind?, limit?)       → [{id, title, excerpt, scope, kind, status, source}]
brain_get(id)                                    → pełny wpis + linki + historia wersji
brain_propose(kind, title, content, scope, links?)  → id propozycji (nigdy approved)
brain_cite(ids[])                                → zapis cytowania
catalogue_show(component?)                       → komponent, zależności, kto zależy
thread_read(conversation_id?, limit?)            → historia (faza 2: tylko conversation turnu)
conversation_post(body)                          → wiadomość w wątku turnu
task_show() / task_blocked(reason) / ask(question)  → odpowiedniki gator-cli
```

Implementacja: oficjalne Go SDK MCP (`github.com/modelcontextprotocol/go-sdk`). Pakiet
`internal/server/mcp` woła te same serwisy co `/agent/*`, a `gator-cli` zostaje dla backendów bez
MCP. Każde wywołanie trafia do `agent_calls` (00024), jak dziś.

Zasoby MCP (`resources`): `brain://project/<slug>/principles`, `brain://me`, czyli do przypięcia w
kliencie człowieka.

## API

```
GET    /brain?q=&scope=&kind=&status=&project=        wyszukiwanie (te same reguły widoczności co MCP)
POST   /brain                                         nowy wpis / propozycja
GET    /brain/{id}                                    wpis + wersje + linki + użycia (citations)
PATCH  /brain/{id}                                    edycja = nowa wersja (supersedes)
POST   /brain/{id}/approve | /reject | /archive
GET    /brain/proposals?project=                      kolejka do przeglądu
GET    /me/brain                                      mój kontekst (ekspertyza, fokus, preferencje)
GET    /projects/{id}/brain/preview?task=             co dostałby job tego taska (debug, jak /knowledge/preview)
```

## Podział na PR-y

1. Migracja 00042, serwis `internal/server/brain` (CRUD, wersje, reguły widoczności z testami).
2. Backfill 00043 i fasady `/product`, `/knowledge` na nowym magazynie.
3. Pakiet dla jobów na Brainie (zamiast product slice + knowledge) i `brain_citations`.
4. Serwer MCP `/mcp` (token joba i usera), wpis w `--mcp-config` runnera, nagrania testowe.
5. Curator rozmów i karta `brain_proposal` w wątku.
6. `brain.propose` dla pluginów i pierwsze użycie: GitHub ADR-y (`docs/adr/*.md`).
7. Kontekst użytkownika: „Ja", wyliczany `focus`, reguła `on_behalf_of`.
8. Sprzeczności i supersede.
9. Ekran Brain w aplikacji (faza 6b).
10. (opcjonalnie) pgvector i wyszukiwanie hybrydowe za flagą, z pomiarem przed/po.

## Gotowe, gdy

- Decyzja ustalona w wątku wraca jako propozycja w ciągu godziny, a po zatwierdzeniu agent w innym
  tasku tego komponentu ją dostaje i cytuje.
- Ktoś podpina Brain do swojego Claude Code i `brain_search` zwraca to samo co aplikacja.
- Prywatna preferencja osoby A nie pojawia się w żadnym pakiecie turnu wywołanego przez B (test).
- Pomiar: mediana `context_bytes` na job nie rośnie o więcej niż 20%, a odsetek odrzuconych faz
  (rollback) spada. To liczymy w `quality`.

## Ryzyka

- **Brain pełen śmieci.** Bez zatwierdzania wszystko trafia do promptu. Dlatego tylko `approved` (z
  wyjątkiem prywatnego `focus`), przegląd nieużywanych wpisów i limit propozycji na wątek.
- **Prywatność.** Kontekst użytkownika to dane osobowe. Musi być eksport i usunięcie całego „mnie"
  (`DELETE /me/brain`) oraz jasna informacja, co widzą agenci.
- **Zmęczenie zatwierdzaniem.** Propozycje grupujemy dziennie w jednej karcie „Brain: 5 propozycji
  z wczoraj", zamiast pięciu kart.

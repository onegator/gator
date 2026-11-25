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
wymaga żadnej nowej zależności. `00045_brain_embeddings.sql` jest opcjonalna: rozszerzenie
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

## Odstępstwa od planu (zapisane przy implementacji)

- **Migracje**: `00042_brain.sql`, `00043_brain_backfill.sql` i `00044_brain_curation.sql` (dokąd
  curator przeczytał każdą rozmowę i który job pisze kurację). Embeddingi mają zarezerwowany numer
  00045 (niezrobione, niżej), a fazy 4–5 przesunęły się o jeden (tabela w `README.md`).
- **`brain_entries` ma kolumny ponad plan**: `lineage` (id pierwszej wersji, wspólne dla wszystkich
  wersji wpisu; trigger wypełnia je przy wstawianiu), `topic` (półka konwencji: language,
  architecture, requirements, testing, security — dawny `knowledge_entries.scope`), `position`,
  `archived_at` i `condensed_from` (kondensacja, jak w 00020), `source_task_id`,
  `source_conversation_id` i `source_private` (skąd przyszła propozycja; typowane kolumny obok
  `source` jsonb, bo zapytania o nie są częste). `visibility = 'private'` jest dozwolone tylko dla
  scope `user` (CHECK). Wersje są unikalne per (scope, projekt, komponent, osoba, agent, kind,
  tytuł) poza konwencjami, jak dawniej w `product_context`. Wyszukiwanie to indeks wyrażeniowy GIN
  na `to_tsvector('simple', title || ' ' || content)`, a nie kolumna `tsv` (jak w 00038: ten sam
  efekt bez wektora w każdym odczycie). `brain_links.target_kind` ma CHECK (entry, component, task,
  conversation); `brain_citations` ma `created_at`.
- **Backfill (00043)** przenosi wiersze z zachowaniem id, statusów, wersji (stają się linią
  `lineage` z `supersedes`), autorów, `source_task_id`, archiwum i kondensacji; `principles` z
  product context to w Brainie `principle`. Stare tabele zostają pod nazwami `product_context_legacy` i
  `knowledge_entries_legacy`, a pod starymi nazwami są **widoki z triggerami INSTEAD OF**, więc SQL,
  który je czyta albo do nich pisze (testy, ręczne zapytania), dalej działa na Brainie.
  `component_decisions` wskazuje teraz na `brain_entries`. Krok down przenosi z powrotem wszystko, co
  Brain ma w kształcie starych tabel (także to, co napisano po migracji), i usuwa to z Brainu: każdy
  wiersz jest w jednym miejscu na każdym kroku. `store/brain_migrations_test.go` robi na osobnej bazie
  wejście na 00041 z product context (wersje, statusy, archiwum, kondensacja), knowledge entries,
  taskami, jobami i decyzją komponentu, up, down do 00041 i znów up, i porównuje dane (JSON całych
  tabel) za każdym razem.
- **Fasady**: `/projects/{id}/product`, `/product/{id}`, `/knowledge`, `/projects/{id}/knowledge`
  (i `gator-cli product propose`, `knowledge search`) idą przez `brain.Legacy` i `brain.Service` z
  tą samą polityką; ich testy przechodzą bez zmian. Wersja zastąpiona nowszą jest w fasadzie
  `approved` (tak czytał ją dawny product context), odrzucona — `archived`. Wpis osoby piszącej przez
  fasadę jest od razu zatwierdzony, jak dawniej.
- **Polityka**: zatwierdza i pisze od razu zatwierdzone członek projektu (nie tylko admin, jak w
  planie) — tak było z product context i tak działa fasada; admin workspace'u dla scope
  `workspace`, osoba dla swojego kontekstu. Każdy człowiek może proponować do workspace'u. Admin
  workspace'u czyta wszystkie projekty, ale **nie** czyta niczyjego prywatnego kontekstu. Agent
  (job) czyta tylko wpisy zatwierdzone i wciąż prawdziwe i proponuje tylko w swoim projekcie
  (project, component, agent). Propozycja z rozmowy prywatnej (DM, kanał prywatny) jest widoczna
  tylko dla tych, którzy mogą czytać tę rozmowę, dopóki ktoś jej nie zatwierdzi, i nigdy nie ma
  cytatu (`source.excerpt` jest czyszczony); tak samo przez `/agent/product` i MCP. Curator nie czyta
  rozmów prywatnych wcale (ich treść nie trafia do instrukcji joba).
- **Pakiet** (`brain.Packer`) ma sekcje: komponent taska i sąsiedzi (zależności i zależni),
  product context (tylko dla ról, które ważą kierunek — researcher, planner, reviewer, curator —
  jak dawniej; worker go nie dostaje), runbooki i fakty projektu, lekcje agenta joba, kontekst
  ludzi (udostępniony + prywatny osoby `on_behalf_of`), wpisy workspace'u spoza półek. Konwencje z
  półek (dawne knowledge entries) idą dalej razem z paczkami wiedzy w `process.Knowledge`, bo paczki
  i wpisy się przesłaniają (ostrzeżenia ekranu Knowledge). Pliki paczek nie są indeksowane jako
  wpisy `source.kind='pack'` — paczki są dalej składane z `knowledge_packs` (filtry roli, fazy i
  tagów), a rodzaj źródła `pack` jest tylko zarezerwowany (wpis z niego jest tylko do odczytu).
  Każdy wpis ma w tytule tag `[brain:<id>]`, a pakiet ma notkę o cytowaniu. Kontekst prywatny osoby
  trafia do promptu, ale w zapisie `job_context` (który czyta projekt) zostaje bez tytułu i treści.
  Job `summary` nie dostaje pakietu w ogóle (i go nie zapisuje).
- **Cytowania**: `packed` przy leasie, `searched` przy `knowledge search`, `brain_search`,
  `brain_get`, `cited` przy końcu joba — serwer (nie runner) wyciąga `[brain:<id>]` z odpowiedzi w
  receipcie, i tylko z tego, co job dostał albo wyszukał (działa z każdą wersją runnera). `brain_cite`
  w MCP zapisuje `cited` dla wpisów, które job może czytać. Przegląd „czy to jeszcze prawda?” to
  `GET /brain/review?days=90`.
- **MCP**: `github.com/modelcontextprotocol/go-sdk` v1.8.0 (buduje się z Go 1.27), serwer
  bezstanowy (`Stateless`, `JSONResponse`): każde żądanie to sesja dla uwierzytelnionego wołającego, z
  tylko tymi narzędziami, które może wołać. Zakresy jobów są w nowym pakiecie
  `internal/server/jobscope`, wspólnym dla `/agent` i MCP: reply dostaje `brain_search`, `brain_get`,
  `brain_cite`, `brain_propose`, `catalogue_show`, `thread_read`, `conversation_post` (bez
  `task_show`, jak `/agent` dla reply), summary nic, faza wszystko. Token joba przechodzi teraz przez
  middleware także pod `/mcp` (i `/mcp/…` na fazę 4). Człowiek dostaje `brain_search`, `brain_get`,
  `brain_propose` (zawsze propozycja), `brain_cite` (nic nie zapisuje), `catalogue_show(project)` i
  zasoby `brain://me`, `brain://project/<slug>/principles`. `thread_read` czyta tylko rozmowę turnu,
  słowa z zewnątrz w znacznikach z nonce. Runner dopisuje serwer `gator` do konfiguracji MCP sesji w
  pliku 0600 poza worktree (token nie trafia do argumentów procesu), obok własnego `--mcp-config`
  runnera, i pozwala sesji `chat` na `mcp__gator`; nagranie `testdata/synthetic-mcp-brain.ndjson` jest
  syntetyczne (bez płatnego modelu).
- **Curator rozmów** (`internal/server/brain/curator`): czyta tylko rozmowy z taskiem (wątki tasków i
  asków) — wątek ludzi bez taska nie ma taska, na którym mógłby iść job. Job kuracji to `kind
  summary` z rolą `curator` (runner v3 zna ten rodzaj: bez narzędzi i tożsamości), żeby nie zmieniać
  protokołu; prompt roli `curator.md` opisuje obie prace. Próg: 30 min ciszy i ≥ 4 wiadomości od
  ostatniej kuracji (albo zamknięcie taska), najwyżej 5 propozycji naraz i 10 czekających z jednej
  rozmowy. Propozycja jest o komponencie taska, gdy task go ma (inaczej o projekcie), z wiadomością i
  cytatem. Karta `brain_proposal` to jedna karta dziennie na task („Brain: N propozycji z …”), z
  approve/reject przy każdej i `replaces` przy sprzeczności; propozycje z rozmów prywatnych kart nie
  mają.
- **Pluginy**: `brain.propose` przyjmuje decision, lesson, runbook, fact, glossary, convention
  (scope project lub component) z obowiązkowym `source_id`, raz na źródło. GitHub proponuje ADR-y z
  `docs/adr/*.md` przy pushu na gałąź bazową, raz na wersję pliku (sha bloba); webhook ma teraz też
  `push`.
- **Kontekst użytkownika**: `GET /me/brain` to eksport (wszystkie wersje) z jednym zdaniem o tym, co
  widzą agenci; `DELETE /me/brain` usuwa wszystko. Ekran „Ja” to faza 6. `focus` jest per projekt,
  liczony co godzinę z otwartych tasków osoby i tych, o których pisała w ich wątku w ostatnim
  tygodniu (źródło `activity`), nowa wersja tylko przy zmianie, archiwizowany, gdy przestaje
  obowiązywać.
- **Sprzeczności**: sprawdzane dla propozycji decyzji (nie dla wpisów zatwierdzonych od razu przez
  człowieka) wobec zatwierdzonych decyzji projektu, przez `Searcher` (FTS: słowa tytułu połączone
  „or”), a „podobna” to ≥ 2 wspólne znaczące słowa tytułu albo 1 i ≥ 25% wspólnych słów treści; inna
  wersja tego samego wpisu, decyzja innego komponentu i ta sama treść to nie sprzeczność.
  Zatwierdzenie propozycji supersede'uje to, czemu przeczy (efekt zatwierdzenia). „Co wiedzieliśmy
  w marcu” to `GET /brain?at=`. Efekty usługi to `brain.Hooks` (propozycja, zatwierdzenie).
- **Pomiar**: `quality.BrainImpact` porównuje dwa okna tej samej długości przed i po chwili: medianę
  `context_bytes` jobów fazy i odsetek cofnięć wśród ruchów faz (rollback / advance+rollback);
  `GET /projects/{id}/brain/impact?pivot=&days=`.
- **Punkt 9**: ekran Brain należy do fazy 6b; `docs/openapi.yaml` opisuje wszystko, czego potrzebuje
  (`/brain`, `/brain/{id}` z wersjami, linkami i użyciami, approve/reject/archive, `/brain/proposals`,
  `/brain/review`, `/me/brain`, `/projects/{id}/brain/preview`, `/projects/{id}/brain/impact`, karta
  `brain_proposal`).
- **Punkt 10 (pgvector) nie wszedł**: wymaga obrazu Postgresa z pgvector i dostawcy embeddingów
  (Voyage albo lokalny model), a testy nie wołają płatnych ani zewnętrznych API; plan i tak każe
  najpierw zmierzyć FTS. Miejsca na niego są gotowe: `brain.Searcher` (wyszukiwanie hybrydowe może
  zastąpić FTS bez zmian wyżej, `brain.Deps.Searcher`) i `brain.Embedder`.

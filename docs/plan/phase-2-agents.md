# Faza 2: Agenci jako członkowie

**Cel:** agent ma tożsamość (`@reviewer`), jest członkiem kanałów i wątków, odpowiada na wzmianki,
a joby procesu (discovery, planning…) są widoczne jako *jego* praca w wątku taska. Agenci mogą
delegować sobie pracę, ale pod limitami.

**Poza zakresem:** instalacja agentów z rejestru (faza 4), agenci zewnętrzni i backendy inne niż
Claude (faza 5), Brain (faza 3; tutaj agent dostaje dzisiejszy kontekst: product slice, knowledge,
working_state).

## Tożsamość agenta

Dzisiejsze role (`internal/server/process/roles/*.md`) stają się **agentami domyślnymi** każdego projektu:
`@researcher`, `@planner`, `@worker`, `@reviewer`, `@security`, `@curator`. Szablon procesu dalej
mówi `role: reviewer`, a serwer rozwiązuje to do agenta projektu z `role_key = 'reviewer'`. Projekt
może podmienić agenta dla roli (np. własny `@backend-reviewer`), nie ruszając szablonu.

### Poziomy uprawnień

| Poziom | Co może | Workspace na runnerze | Consent |
|---|---|---|---|
| `chat` | czytać repo, szukać, odpowiadać, proponować wiedzę, tworzyć taski | worktree tylko do odczytu, narzędzia ograniczone (bez Edit/Write/Bash poza listą) | wzmianka od członka projektu = zgoda na ten jeden turn |
| `work` | zmieniać kod na gałęzi `gator/…`, uruchamiać komendy | pełny worktree, jak dziś | dzisiejszy `agents_allowed_at` na tasku (00033) |

Gdy agent `work` dostaje wzmiankę w rozmowie bez taska albo w tasku bez zgody, odpowiada w trybie
`chat`, a w wątku pojawia się karta **„Pozwolić @worker pracować nad tym?"**. Akceptacja robi
`allow-agents` i uruchamia turn `work`. Nic nie omija dzisiejszej reguły consentu.

Ograniczenie narzędzi dla `chat` w backendzie Claude: `--permission-mode` z jawną listą
`--allowedTools` (Read, Grep, Glob, WebFetch, narzędzia MCP Gatora) i `--disallowedTools` dla reszty.
Do zweryfikowania na aktualnej wersji CLI i nagrania w `testdata`.

## Dane (`00039_agents.sql`, `00040_agent_turns.sql`)

```sql
CREATE TABLE agents (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid REFERENCES projects(id) ON DELETE CASCADE,  -- NULL = agent workspace'u
    handle        text NOT NULL,                 -- [a-z0-9-]{2,32}, unikalny w projekcie razem z workspace'owymi
    display_name  text NOT NULL,
    description   text NOT NULL DEFAULT '',      -- jedna linia, widoczna w @autocomplete
    avatar        text NOT NULL DEFAULT '',      -- SF Symbol, emoji albo url
    role_key      text,                          -- researcher|planner|worker|reviewer|… (do szablonów procesu)
    instructions  text NOT NULL DEFAULT '',      -- pusty = prompt roli z internal/server/process/roles
    tier          text NOT NULL DEFAULT 'chat' CHECK (tier IN ('chat', 'work')),
    policy        jsonb NOT NULL DEFAULT '{}',   -- {backend, model, max_cost_usd}; brak = process_config.policy
    tools         jsonb NOT NULL DEFAULT '[]',   -- nazwy serwerów MCP / pluginów (faza 4)
    skills        jsonb NOT NULL DEFAULT '[]',   -- faza 4
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused')),
    source        jsonb NOT NULL DEFAULT '{"kind":"builtin"}', -- builtin | custom | marketplace{item,version}
    created_by    uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX agents_handle_idx ON agents (coalesce(project_id, '00000000-0000-0000-0000-000000000000'), handle);

-- Jeden turn = jedna odpowiedź agenta na jedno wywołanie (wzmianka, DM, delegacja, faza procesu).
CREATE TABLE agent_turns (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id         uuid NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    conversation_id  uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    trigger_message_id uuid REFERENCES messages(id) ON DELETE SET NULL,
    reply_message_id uuid REFERENCES messages(id) ON DELETE SET NULL,
    requested_by_kind text NOT NULL,             -- user | agent | automation | process
    requested_by     uuid,
    on_behalf_of     uuid REFERENCES users(id),  -- człowiek na początku łańcucha (faza 3: jego prywatny kontekst)
    chain_id         uuid NOT NULL,              -- wspólny dla delegacji agent→agent
    hop              smallint NOT NULL DEFAULT 0,
    job_id           uuid REFERENCES jobs(id) ON DELETE SET NULL,
    status           text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','done','failed','refused','stopped')),
    refusal          text,                        -- np. 'hop_limit', 'budget', 'paused', 'no_consent'
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_id, trigger_message_id)        -- redelivery wzmianki nie robi drugiej odpowiedzi
);

ALTER TABLE jobs
    ADD COLUMN agent_id uuid REFERENCES agents(id) ON DELETE SET NULL,
    ADD COLUMN turn_id  uuid REFERENCES agent_turns(id) ON DELETE SET NULL,
    ADD COLUMN tier     text NOT NULL DEFAULT 'work';
```

Migracja 00039 tworzy agentów domyślnych dla istniejących projektów (z `role_key` i pustym
`instructions`), a `jobs.agent_id` uzupełnia po `role`.

## Rozmowa bez taska: kind `ask`

Każdy job potrzebuje taska (`jobs.task_id NOT NULL`, token joba ma zakres taska, budżety i metryki
liczą się per task). Zamiast to rozluźniać, dodajemy szablon `internal/server/process/defaults/ask.yaml`:

```yaml
kind: ask
hidden: true           # nie pokazuje się na Boardzie ani w metrykach „feature lead time”
phases:
  - name: answer
    owner: runner
    gate: auto
    timeout: 1h
  - name: closed
    owner: human
    gate: auto
```

Wzmianka agenta w wątku bez taska tworzy (albo wznawia, jeśli wątek ma już `ask`) task `ask` przypięty
do tego wątku. „Zrób z tego task" na takim wątku **zmienia kind** `ask` na `feature` lub `bug` i
wchodzi w pierwszą fazę. Historia, koszt i working_state zostają.

## Przebieg turnu

1. `message.posted` z wzmianką agenta (albo wiadomość w DM z agentem) trafia do konsumenta
   `internal/server/agents/dispatcher.go`.
2. Sprawdzenia: agent `active`; autor może pisać w conversation; hop < 4 w `chain_id`; dzienny budżet
   projektu; budżet łańcucha (domyślnie 2× `max_cost_usd` agenta). Odmowa zapisuje się jako `refused`
   z krótką wiadomością systemową w wątku („@planner jest wstrzymany").
3. Turn tworzy job (`kind` w bounds: `reply`, `tier` z reguł wyżej, `agent_id`, `turn_id`), a w wątku od
   razu pojawia się wiadomość agenta `status='streaming'` z kartą postępu.
4. **Pakiet rozmowy** (`conversations/context.go`), na wzór OpenBota, ale nasz:
   - na początku zadanie (wiadomość wywołująca i kto prosi),
   - streszczenie starszej części wątku (tło, cache w `conversation_summaries`),
   - ostatnie wiadomości dosłownie do budżetu bajtów,
   - kontekst taska (opis, artefakty, working_state, product slice) jak w dzisiejszym leasie,
   - na końcu zadanie powtórzone krótko.

   Wiadomości od `external` / `plugin` są w znacznikach untrusted. Rozmiar trafia do
   `context_bytes` / `context_docs`.
5. Runner strumieniuje eventy, a serwer składa z `text` treść wiadomości i aktualizuje ją co ~500 ms
   (`message.edited`). Tool calle idą do karty postępu, nie do treści.
6. Finish ustawia wiadomość na `final`, a pod nią pojawia się koszt i czas. `gator-digest` działa jak
   dziś i przebudowuje working_state taska `ask`.
7. Steer: odpowiedź człowieka w wątku *w trakcie* turnu tego agenta to `steer` (dzisiejszy
   `/jobs/{id}/steer` z `--resume`), a nie nowy turn.

### Delegacja agent→agent

Agent może:
- napisać w wątku przez narzędzie `conversation.post` (gator-cli `say`, a w fazie 3 MCP), z
  wzmianką innego agenta. To nowy turn z tym samym `chain_id`, `hop + 1` i `requested_by_kind = 'agent'`;
- poprosić o człowieka: `gator-cli ask` daje kartę `decision` z pytaniem w wątku.

Limity: hop ≤ 4, budżet łańcucha, ten sam agent nie dwa razy z rzędu w jednym łańcuchu bez człowieka
pomiędzy, a delegacja z `chat` do agenta `work` wymaga consentu jak wyżej. Każdy przypadek jest testem.

### Joby procesu jako praca agenta

Joby z autopilota dostają `agent_id` agenta roli, a karta `job` w wątku taska pokazuje autora
`@planner`. Nic więcej się nie zmienia w autopilocie. Automations (PR review, error triage…) też
mają autora-agenta: `@reviewer`, `@security`, `@triage` (nowy domyślny), `@reporter`.

## Zmiany w protokole runnera

`internal/proto`: wersja N+1 (serwer mówi N i N-1, więc stare runnery działają, ale dostają tylko
joby `tier='work'` bez kontekstu rozmowy):

- `Job.Tier` (`chat` | `work`) i `Job.Agent {handle, display_name}`, żeby runner mógł ustawić
  ograniczenia narzędzi i podpisać commit (`Co-Authored-By: @reviewer via Gator`);
- `Job.Kind` (`phase` | `reply`), bo dla `reply` runner nie wymaga commitów i nie pushuje pustej gałęzi;
- dla `chat` runner robi checkout tylko do odczytu (`git worktree add --detach` plus `chmod -R a-w`
  albo osobny katalog bez prawa zapisu dla procesu agenta).

## Agent-tools (`gator-cli`)

Nowe komendy w zakresie tokenu joba:

```
gator-cli say --file -                 napisz w wątku turnu (dodatkowa wiadomość, np. pytanie do @planner)
gator-cli thread show [--limit N]      więcej historii wątku niż w pakiecie
gator-cli task create --kind … --title … --file -   zaproponuj task (wymaga potwierdzenia: karta)
```

## API

```
GET    /projects/{projectId}/agents                 lista (z domyślnymi), do @autocomplete
POST   /projects/{projectId}/agents                 własny agent {handle, display_name, instructions, tier, policy}
GET    /agents/{agentId}                            profil: opis, statystyki (turny, koszt 30 dni, % zaakceptowanych)
PATCH  /agents/{agentId}                            edycja, pauza
DELETE /agents/{agentId}                            tylko custom; builtin można wstrzymać
GET    /agents/{agentId}/turns?since=               historia
POST   /turns/{turnId}/stop
```

Uprawnienia: agentów projektu tworzy i edytuje admin projektu; członek widzi i wzmiankuje.
Agenci workspace'u są tylko dla admina workspace'u.

## Podział na PR-y

1. Migracje 00039/00040, seed agentów domyślnych, rozwiązywanie `role → agent` w autopilocie.
2. `ask.yaml` (ukryty kind) i promote `ask → feature/bug`.
3. Dispatcher wzmianek z limitami (hop, budżet, pauza, consent) i testami tabelarycznymi.
4. Pakiet rozmowy i streszczenia (`conversation_summaries`, generowane tanim modelem z policy
   `roles.summarizer`).
5. Proto N+1: tier, kind, agent; runner w trybie read-only dla `chat`; nagrania `testdata`.
6. Strumieniowanie odpowiedzi do wiadomości i karta postępu.
7. Steer z wątku, Stop z wiadomości.
8. `gator-cli say`, `thread show`, `task create`.
9. Karta consentu „Pozwolić @worker…".
10. Endpointy agentów i ekran profilu agenta (faza 6a).

## Gotowe, gdy

- „@reviewer zobacz PR #12" w `#general` daje odpowiedź w wątku w trybie `chat`, z kosztem, i nic
  nie zostało zapisane w repo.
- „@worker napraw to" w wątku bez zgody daje kartę consentu; po akceptacji jest gałąź z commitem.
- Joby autopilota są podpisane agentem roli.
- Łańcuch @planner → @worker → @reviewer zatrzymuje się na hop 4 albo na budżecie z czytelnym
  komunikatem.

## Ryzyka

- **Koszt rozmów.** Każda wzmianka to sesja agenta. Pomagają tańszy model dla `chat` w policy
  (`policy.tiers.chat.model`), dzienne limity per agent i widoczny koszt pod każdą odpowiedzią.
- **Latencja.** Start sesji Claude trwa kilka sekund. Wiadomość `streaming` z „@reviewer czyta wątek…"
  pojawia się natychmiast. Docelowo ciepłe sesje per wątek (`--resume` ostatniej sesji agenta w
  wątku) to optymalizacja na fazę 3+.
- **Read-only na prawdę.** Ograniczenie narzędzi CLI to nie sandbox. Uczciwie opisujemy w UI, że
  `chat` = „nie ma narzędzi do zapisu", a nie „nie może zapisać".

## Odstępstwa od planu (zapisane przy implementacji)

- **Migracje** mają numery z planu: `00039_agents.sql` (agenci, agenci domyślni, `jobs.agent_id`) i
  `00040_agent_turns.sql` (turny, `jobs.turn_id/tier/kind`, `conversation_summaries`). Każda ma krok
  down; `internal/server/store/migrations_test.go` robi na osobnej bazie wejście na 00038, dane
  (projekty, taski, joby z rolami), up, down do 00038 i znów up, i sprawdza backfille za każdym razem.
- **Unikalność uchwytu** pilnuje trigger `agents_handle_visible_once` (BEFORE INSERT/UPDATE): bierze
  blokadę doradczą na uchwyt i odrzuca (`unique_violation`) uchwyt, który byłby widoczny w projekcie
  dwa razy — projektowy wobec workspace'owego i odwrotnie. Indeks `agents_handle_idx` jest
  `(project_id, handle) NULLS NOT DISTINCT`. Skutek: agent workspace'u nie może mieć uchwytu, który ma
  jakikolwiek projekt (np. `@reviewer`). Testy: kolejność w obie strony i równoległe tworzenie.
- **Agenci domyślni** powstają w bazie: funkcja `gator_seed_default_agents(project)` i trigger
  `AFTER INSERT ON projects`, a migracja woła ją dla istniejących projektów. Projekt zrobiony
  dowolną drogą (API, provisioning, test) ma ich od razu. Lista: `@researcher`, `@planner`, `@worker`,
  `@reviewer`, `@security` (`security_reviewer`), `@curator`, `@triage` i `@reporter`; dwaj ostatni
  dostali prompty ról `roles/triage.md` i `roles/reporter.md`, a `agents.Defaults` w Go jest
  sprawdzane testem wobec bazy. Tier `work` mają `@worker` i `@triage`, reszta `chat`.
- **Rola → agent**: `AgentForRole` wybiera agenta projektu przed workspace'owym i agenta zrobionego
  przez ludzi przed wbudowanym. Prompt joba to `instructions` agenta, a gdy puste — prompt jego roli.
  **Automations** mają autora (`Definition.Author`, w API `author`); error triage robi implementację
  incydentu agentem `@triage` (`Instead: worker`), dalsze fazy zostają przy agentach ról. Weekly summary
  pisze serwer, więc `@reporter` jest tylko jego autorem.
- **`jobs.kind`** (`phase | reply | summary`) jest nową kolumną, bo plan miał `Job.Kind` tylko w proto.
  Autopilot, inbox i blokada zmiany kindu patrzą tylko na joby `phase`: odpowiedź agenta w wątku nie
  jest pracą fazy. Reply i summary nie zostawiają artefaktu fazy; reply robi digest i przebudowuje
  `working_state`, summary niczego. Turn ending (sprzeciwy pluginów) dotyczy tylko jobów `phase`.
- **`agent_turns`** ma kilka kolumn ponad plan: `project_id`, `parent_turn_id` (kto delegował),
  `escalated_from` (turn chat, któremu człowiek pozwolił pracować), `tier`, `updated_at`,
  `finished_at`; CHECK-i na `requested_by_kind` i na zgodność `status='refused'` z `refusal`.
- **Ask**: faza `answer` ma `gate: human` i `timeout: 0` zamiast `auto`/`1h` — ukryty task zablokowany
  przez timeout nikomu by się nie pokazał, a test „każda faza ma wyjście” wymaga, by z fazy `auto`
  wychodził timeout. Faza runnera w szablonie `hidden` nie potrzebuje roli. Wątek pod wiadomością
  staje się wątkiem aska (`kind='task'`); DM nie może być rozmową taska, więc ask DM-a ma
  `external_refs.conversation`, a jego karty idą do DM-a. Ask nie ma karty `task` ani `decision`, nie
  jest na Boardzie, w inboxie ani w liście szablonów; człowiek nie tworzy go przez API (400).
  „Zrób z tego task” na wątku aska to `process.PromoteAsk` (zmiana kindu mimo wykonanej pracy).
  Ask w prywatnym kanale idzie dotychczasową ścieżką „apart” (nowy task), by wątek taska był
  czytelny dla projektu.
- **Wzmianki agentów** przychodzą jawnie od klienta (`mentions: [{kind: "agent", id}]`), jak ludzi w
  fazie 1. Agent piszący przez `gator-cli say` wzmiankuje uchwytem `@handle` w treści — uchwyty są
  unikalne tam, gdzie je widać. Delegacja to wiadomość agenta z `origin.turn`; łańcuch, hop i
  `on_behalf_of` biorą się z tego turnu. Turn wywołują tylko teksty ludzi i agentów (nie karty,
  system, pluginy, external). Zdarzenie `message.posted` ma osobno `mentions` (ludzie) i `agents`.
- **Agent w rozmowie**: dołącza jako członek wątku, w którym odpowiada; `PUT /conversations/{id}/
  members/agent/{id}` dodaje go do kanału; DM z agentem to `POST /conversations/dm` z `agents`
  (tylko w projekcie) — każda wiadomość człowieka w takim DM-ie pyta agenta.
- **Limity** to osobne strażniki (`agents.Guard`), pytane po kolei: wstrzymany, czy człowiek może
  pisać w rozmowie, hop, ten sam agent dwa razy z rzędu, budżet łańcucha, dzienny budżet projektu.
  **Hop**: łańcuch ma najwyżej 4 turny (hop 0–3); turn z hopem 4 jest odrzucany („zatrzymuje się na hop
  4”). **Budżet łańcucha** to 2× `max_cost_usd` turnu proszonego agenta (polityka roli, tieru i
  własna); agent bez limitu nie ma limitu łańcucha (hop i tak obowiązuje); liczy się koszt
  zakończonych jobów łańcucha. **Ten sam agent dwa razy z rzędu** = agent wzmiankujący sam siebie
  ze swojego turnu. **chat → work**: agent `work` pracuje tylko na tasku ze zgodą (00033), kto
  by go nie poprosił; inaczej odpowiada w `chat` i pojawia się karta consentu. Odpowiedź człowieka
  zaczyna nowy łańcuch. Odmowa to turn `refused` z kodem i linią systemową w rozmowie, raz.
- **Polityka turnu**: `policy.tiers.<tier>` (np. `chat.model`) leży nad `policy.roles.<rola>`, a nad
  nimi `agents.policy` agenta (`process.PolicyForTurn`); joby faz biorą politykę roli i agenta, bez
  tierów. Summarizer to rola `summarizer` (`policy.roles.summarizer`, prompt `roles/summarizer.md`).
- **Pakiet rozmowy** (`conversations/context.go`) niesie też wiadomości dopisane po pytaniu, zanim
  job wystartował (poprawka do jeszcze nie uruchomionego turnu nie wymaga steera). Wątek zaczyna się
  od wiadomości-korzenia. Streszczenie pisze job `summary` na tym samym tasku, jeden naraz na
  rozmowę; do czasu, aż jest, tło to pierwsze słowa każdej starszej wiadomości. Pakiet idzie do
  runnera w `proto.Job.Turn` (pytanie, rozmowa, pytanie znowu), a w `job_context` jest zapisany z
  `origin: conversation` i liczony w `context_bytes/docs`.
- **Proto v3**: serwer mówi v3 i v2 (v1 już nie). Runner v2 dostaje tylko joby `phase` (a nie „joby
  `work` bez kontekstu rozmowy”): odpowiedź `work` uruchomiona jak faza commitowałaby i pushowała bez
  rozmowy. Odpowiedź, którą mogłyby wziąć tylko runnery v2, jest oznaczana jako `no_runner`.
- **Read-only `chat`** w Claude Code 2.1.331 (sprawdzone z `claude --help` i nagrane w
  `testdata/claude-2.1.331-chat-*.ndjson`): `--permission-mode dontAsk --tools Read,Grep,Glob,WebFetch,Bash
  --allowedTools Read,Grep,Glob,WebFetch,Bash(gator-cli:*) --disallowedTools Edit,Write,NotebookEdit`.
  Nagranie pokazuje, że Write jest niedostępny, `touch` przez Bash odrzucony, a `gator-cli` działa.
  Workspace `chat` to `git worktree add --detach` bez gałęzi, z plikami i katalogami bez prawa zapisu;
  mapowanie tieru na dostęp jest w `executor/access.go`, flagi w `backend/claude/access.go`.
  **Podpis commitów** daje hook `prepare-commit-msg` spoza checkoutu (przez `GIT_CONFIG_*` w
  środowisku agenta): `Co-Authored-By: @worker via Gator`, raz na commit, w każdym jobie z agentem.
- **Strumieniowanie** czyta tekst z `job_events` (prawda w bazie; hub tylko budzi), co 500 ms
  przepisuje wiadomość agenta (`message.edited`, bez digestu) i wysyła `job.progress`, po którym
  karta `job` pokazuje liczbę tool calli i ostatni. Koniec turnu (`job.finished` z outboxu) ustawia
  wiadomość na `final` (odpowiedź), `failed` (dlaczego jej nie ma) albo `final` z dopiskiem przy
  zatrzymaniu. Wiadomość ma w API pole `turn` (status, tier, koszt, czas) — to „koszt pod wiadomością”.
  Przepisywana jest tylko wiadomość `streaming`.
- **Steer**: wiadomość człowieka w rozmowie z aktywnym turnem steruje nim, gdy nie wzmiankuje agentów
  albo wzmiankuje odpowiadającego; innych wzmiankowanych pyta się jak zwykle. Słowa agentów nigdy
  nie sterują. **Stop**: `POST /turns/{id}/stop` (każdy, kto może pisać w rozmowie); `GET /turns/{id}`.
- **`gator-cli task create`** daje kartę `task_proposal`; task tworzy człowiek przez nowy
  `POST /proposals/{messageId}/accept` (plan nie nazywał endpointu). `say` działa też w jobie fazy (w
  wątku taska, jako nowy łańcuch).
- **Karta consentu** ma klucz `consent:<turn>`, a jej przycisk to nowy `POST /turns/{id}/allow`
  (plan mówił tylko „akceptacja robi allow-agents”): zgoda na tasku w imieniu klikającego członka
  projektu i turn `work` agenta (`escalated_from`, wyzwalaczem jest karta), przez te same strażniki.
- **Pauza** dotyczy turnów w rozmowach; joby faz autopilota dalej idą (wyłącza je
  `autopilot.enabled`), bo wstrzymany agent roli zatrzymałby task po cichu.
- **Profil agenta** (`GET /agents/{id}`) zamiast „% zaakceptowanych” podaje `doneShare` (część
  zakończonej pracy, która skończyła się `done`) — artefakty nie są jeszcze powiązane z jobem, który
  je zrobił. Doszły `GET/POST /workspace/agents` dla agentów workspace'u (tylko admin workspace'u
  tworzy i edytuje). Wbudowany agent zachowuje uchwyt i rolę; usuwa się tylko własnych (409 dla
  wbudowanych).
- **Start turnu** trzyma turn blokadą doradczą na osobnym połączeniu, a nie blokadą wiersza: zapisy
  startu odwołują się do turnu i rozmowy, a blokada wiersza czekałaby sama na siebie. Dwa dispatchery
  (dwa serwery, powtórzone zdarzenie) nie startują więc jednego turnu dwa razy.
- **Punkt 10**: ekran profilu agenta należy do fazy 6a; `docs/openapi.yaml` opisuje wszystko, czego
  potrzebuje (`Agent`, `AgentProfile`, `AgentTurn`, `MessageTurn`, karty `consent` i `task_proposal`).

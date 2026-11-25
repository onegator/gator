# Faza 5: Otwarcie na zewnątrz

**Cel:** Gator nie zamyka zespołu w jednym narzędziu ani u jednego dostawcy modeli.
1. Runner uruchamia dowolnego agenta mówiącego **ACP** (Codex, Gemini, Copilot, OpenCode…), a nie
   tylko `claude -p`.
2. Zewnętrzni agenci mogą być członkami workspace'u przez **A2A**, a nasi agenci mogą wywoływać
   zewnętrznych.
3. Zespół, który żyje w **Slacku** (potem Teams, Telegram), rozmawia z agentami Gatora stamtąd, a
   rozmowa i tak ląduje w Gatorze.
4. **Komentarze w GitHubie** (`@gator-reviewer …` w PR) są wejściem do wątku.

## 5.1 Backend ACP w runnerze

ACP (Agent Client Protocol) to JSON-RPC po stdio między klientem a agentem: sesja, prompt,
strumień aktualizacji, prośby o uprawnienia. Dziś `internal/runner/backend` ma tylko `claude`.
Dodajemy `backend/acp`, który implementuje ten sam interfejs `Backend` (`Name`, `Run`, `AuthState`):

| Element `Spec` / `Outcome` | Mapowanie na ACP |
|---|---|
| start sesji w worktree | `initialize` → `session/new {cwd, mcpServers}` (tu Brain MCP i proxy MCP z fazy 3/4) |
| prompt (pakiet roli + rozmowy) | `session/prompt` |
| eventy `text`, `tool_call`, `tool_result` | powiadomienia `session/update` |
| steer | `session/cancel` + kolejny `session/prompt` w tej samej sesji |
| bounds (timeout, limit tool calli) | runner liczy sam i ubija grupę procesów, jak dziś |
| `tier=chat` | odpowiedź na `session/request_permission`: odmowa dla zapisu/exec; `work`: zgoda zgodna z bounds |
| usage / koszt | z aktualizacji, jeśli agent raportuje; inaczej `costEstimated` z czasu i modelu |

Konfiguracja runnera (`runner.env`): `GATOR_ACP_AGENTS=codex:/usr/local/bin/codex-acp,gemini:…`.
Każdy staje się backendem `acp:<name>` z własnym `AuthState` (komenda `--version` i sprawdzenie
logowania specyficzne dla agenta). Policy (`process_config.policy`) wybiera `backend: acp:codex`
per rola albo per agent. Dalej obowiązuje zasada „bez cichej zamiany": job czeka na runner z
zalogowanym backendem.

Przed implementacją trzeba sprawdzić aktualny stan adapterów ACP dla każdego CLI (wersje, flagi,
czy raportują usage). To się szybko zmienia. Każdy wspierany agent dostaje nagranie w
`backend/acp/testdata/<agent>` i testy replay jak dla Claude.

## 5.2 A2A: agenci spoza Gatora

A2A (Agent2Agent, projekt Linux Foundation) opisuje agenta przez **Agent Card** (nazwa,
umiejętności, endpoint, auth) i zlecanie mu zadań z cyklem życia i strumieniem wyników.

**Inbound (ktoś woła agenta Gatora):**
- gator-server publikuje Agent Card dla agentów projektu oznaczonych `a2a_exposed` (domyślnie nikt);
- zadanie A2A tworzy turn (faza 2) z `requested_by_kind='external'` w dedykowanym kanale
  `#a2a-<caller>` projektu, więc ludzie widzą, kto i o co prosił;
- autoryzacja tokenem `gtr_a2a_…` mintowanym przez admina projektu, z rate limitem i budżetem
  dziennym per klient. Treść zlecenia jest untrusted.

**Outbound (agent Gatora woła zewnętrznego):**
- agent `kind='remote'` w tabeli `agents` (handle, url Agent Card, sekret w keyringu); wzmianka
  `@vendor-agent` w wątku idzie do niego jako zadanie A2A, a odpowiedzi wracają jako wiadomości
  `author_kind='agent'` z oznaczeniem „zewnętrzny";
- delegacja agent→agent z fazy 2 obejmuje remote, z tymi samymi limitami hop i budżetu.

```sql
-- 00049_external_agents.sql (w planie 00048; zob. odstępstwa)
ALTER TABLE agents ADD COLUMN kind text NOT NULL DEFAULT 'local' CHECK (kind IN ('local','remote')),
                   ADD COLUMN remote jsonb,          -- {card_url, auth, secrets_ref}
                   ADD COLUMN a2a_exposed boolean NOT NULL DEFAULT false;
CREATE TABLE a2a_clients (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name text NOT NULL, token_id uuid NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
    daily_budget_usd numeric NOT NULL DEFAULT 5,
    created_by uuid REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now()
);
```

Wersję specyfikacji A2A i ścieżkę Agent Card (well-known) ustalamy przy implementacji. Bierzemy
aktualne SDK albo piszemy cienką warstwę na JSON-RPC, jeśli Go SDK nie będzie dojrzałe.

## 5.3 Mosty komunikatorów

Most to **plugin** z nową capability `messaging`, czyli bez kodu Slacka w core. Core dostaje:

**Nowe core methods (plugin → core):**
```
conversation.find_or_create {external_ref, kind, name}        → conversation_id
message.post {conversation_id, body, author: {external_id, display_name}, external_ref}
message.update {external_ref, body}
member.link {external_id, email}                               → user_id | null (powiązanie konta)
```

**Nowe hooki (core → plugin):**
```
messagePosted {conversation, message}     wiadomość z Gatora (człowiek albo agent) do odbicia na zewnątrz
messageEdited {…}                          np. strumieniowana odpowiedź agenta, aktualizowana na zewnątrz co ~2 s
cardAction?                                przyciski Approve/Rollback w Slacku (Block Kit) → endpointy taska
```

**Mapowanie Slack ↔ Gator:**

| Slack | Gator |
|---|---|
| kanał podłączony do projektu | `channel` z `origin.slack_channel` |
| wątek | `thread` (lub `task`, gdy wątek został promowany) |
| `@Gator reviewer …` albo `/gator ask @reviewer …` | wzmianka agenta → turn |
| przyciski w wiadomości | akcje kart `decision` (tylko powiązane konta z uprawnieniem) |
| użytkownik Slacka bez konta Gatora | `author_kind='external'`; agent go słucha, ale nie może zatwierdzać ani dawać consentu `work` |

```sql
-- 00048_messaging_bridges.sql (w planie 00049; zob. odstępstwa)
CREATE TABLE external_identities (
    provider text NOT NULL, external_id text NOT NULL,
    user_id uuid REFERENCES users(id) ON DELETE CASCADE,
    display_name text NOT NULL,
    PRIMARY KEY (provider, external_id)
);
CREATE TABLE external_message_refs (
    provider text NOT NULL, external_ref text NOT NULL,
    message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    PRIMARY KEY (provider, external_ref)
);
```

Idempotencja: Slack ponawia eventy, więc `external_message_refs` + `webhook_deliveries`. Pętla echa:
wiadomość z `origin.provider = X` nie wraca do pluginu X.

Kolejność: **Slack** (najważniejszy dla zespołów software'owych), potem **Microsoft Teams**, potem
**Telegram/Discord** (społeczności, OSS).

## 5.4 GitHub jako wejście rozmowy

Rozszerzenie istniejącego pluginu `github`:
- komentarz w PR/issue z `@gator <handle> …` (albo wzmianka aplikacji GitHub) trafia jako
  `message.post` do wątku taska powiązanego z PR (`external_refs`), a jeśli go brak, do nowego `ask`;
- odpowiedź agenta wraca jako komentarz (skrócony, z linkiem do pełnego wątku w Gatorze);
- review agenta jako GitHub review z komentarzami w liniach, gdy agent zwróci je strukturalnie w
  digest (`review_comments`).

## Podział na PR-y

1. `backend/acp`: klient JSON-RPC stdio, mapowanie eventów, permission policy dla tier.
2. ACP: pierwszy agent (Codex lub Gemini) z nagraniem i testami replay; `AuthState`.
3. Policy: `backend: acp:<name>` per rola/agent; UI wyboru backendu w profilu agenta.
4. Core methods i hooki `messaging` w `plugin/` (SDK + `plugintest`).
5. Plugin `slack` (OAuth install, Events API, Block Kit, link kont).
6. GitHub: komentarze → wątek i odpowiedzi → komentarz.
7. A2A outbound (`agents.kind='remote'`).
8. A2A inbound (Agent Card, `a2a_clients`, kanał `#a2a-*`).
9. Teams / Telegram jako kolejne pluginy (po walidacji Slacka).

## Gotowe, gdy

- Ten sam task przechodzi discovery na Claude i implementację na Codexie przez ACP, a working_state
  przechodzi między nimi (już dziś jest niezależny od backendu).
- „@Gator reviewer zobacz #12" w Slacku daje odpowiedź w wątku Slacka i ten sam wątek w Gatorze.
  Approve w Slacku działa tylko dla powiązanego konta z rolą member+.
- Zewnętrzny agent A2A dostaje zadanie i odpowiada, a koszt i limity są widoczne.

## Ryzyka

- **Rozjazd adapterów ACP.** Każdy CLI implementuje ACP trochę inaczej. Wspieramy jawną listę, każdy
  z nagraniem, a reszta jest „eksperymentalna".
- **Most do Slacka spowalnia przejście na Gatora.** To świadomy wybór: najpierw obecność tam, gdzie
  zespół jest. Rozmowa i tak żyje w Gatorze (Brain, karty, metryki), Slack jest oknem.
- **Zewnętrzni autorzy.** Wszystko z mostów i A2A to untrusted. Żaden external nie zatwierdza bramek
  i nie daje consentu `work`.

## Odstępstwa od planu (zapisane przy implementacji)

- **Migracje** zamieniły się numerami: mosty powstały przed A2A (kolejność PR-ów 4–6 przed 7–8), więc
  `00048_messaging_bridges.sql` i `00049_external_agents.sql` (tabela w `README.md`). 00045 dalej czeka
  na embeddingi. `store/external_migrations_test.go` robi na osobnej bazie: wejście na 00047 z
  rozmową, wiadomością i turnem, up, dane obu migracji (tożsamości, odwołania, kody, turn z zewnątrz,
  agent zdalny z wywołaniem, klient A2A z tokenem i taskiem), down do 00047 i znów up; turn zamówiony z
  zewnątrz zostaje po down jako `process`, a tokeny `a2a` i agenci zdalni (z ich turnami) znikają.
- **Tożsamości są projektu**: `external_identities` ma `id`, `project_id` i `UNIQUE (project_id,
  provider, external_id)` zamiast klucza `(provider, external_id)` — ten sam użytkownik Slacka w dwóch
  projektach to dwie tożsamości, a powiązanie w jednym nic nie daje w drugim. `provider` ustawia core
  (nazwa pluginu), nigdy plugin; `external_id` jest w przestrzeni pluginu i zawiera workspace (`T…/U…`).
  `external_message_refs` też jest per projekt. Doszły `external_conversation_refs` (gdzie rozmowa
  żyje na zewnątrz; jedno miejsce na providera) i `external_link_codes`.
- **`member.link` bierze kod, nie e-mail**: osoba robi w Gatorze kod (`POST
  /projects/{id}/external-links`, `GTR-XXXX-…`, 80 bitów, 10 minut, raz, dla projektu i pluginu,
  trzymany jako hash, najwyżej 10 na godzinę) i wpisuje go tam, gdzie słucha most (`/gator link …`).
  Adres e-mail nigdy nie wiąże konta: administrator workspace'u Slacka może nadać kontu dowolny adres.
  Próby kodu są ograniczone per konto. Powiązana osoba, która nie może pisać w rozmowie, pisze znów
  jako tożsamość z zewnątrz. Lista i odpięcie: `GET /projects/{id}/external-identities`, `DELETE
  /external-identities/{id}/link`.
- **`cardAction` jest metodą core (`card.action`), nie hookiem**: przycisk wciska się na zewnątrz, więc
  to plugin mówi core'owi. Działa tylko `approve` na oczekującej karcie `decision`, którą ten most
  dostał, dla powiązanego konta z rolą member+ (`process.Approve` w imieniu osoby). Rollback (potrzebuje
  fazy i powodu) i consent zostają w Gatorze; karta linkuje do wątku.
- **Słowa z zewnątrz**: wiadomość tożsamości z zewnątrz pyta agentów (dispatcher), ale turn jest
  `requested_by_kind='external'` (nowa wartość CHECK w 00048), zawsze `chat` (`agents.GrantFor`), bez
  karty consentu i bez możliwości `POST /turns/{id}/allow`, nawet na tasku ze zgodą na agentów.
  Przy tym wyszła luka z fazy 2: pytanie (`Ask`) briefu nie było w znacznikach untrusted, a `Repeat`
  powtarzało pierwsze zdanie bez nich — teraz pytanie z zewnątrz jest ogrodzone, a `Repeat` go nie
  cytuje. W pakiecie autor z zewnątrz to „someone on <provider>, outside the workspace”, bez nazwy,
  którą sam sobie dał (nazwa wyświetlana to też słowa z zewnątrz).
- **Echo i prywatność**: `origin` wiadomości ma `provider` i `external_ref`; most nie dostaje tego, co
  sam przyniósł. Rozmowa prywatna (DM, kanał prywatny) nigdy nie jest podłączana ani odbijana;
  most pisze tylko w rozmowach, które sam podłączył (albo w wątkach pod nimi). Odpowiedź agenta idzie
  na zewnątrz najwyżej co 2 s (`bridges.EditEvery`), końcowa zawsze; hook niesie `reply_to` (zewnętrzne
  odwołanie pytania, gdy przyszło tym mostem).
- **Rozszerzenia hosta pluginów**: metody `messaging` i hooki z outboxu są rejestrowane
  (`plugins.Config.Extensions`, `EventHooks`), a nie dopisywane do switcha w `core.go`; metoda
  rozszerzenia jest tylko dla pluginu z jego capability. Webhook może odpowiedzieć nadawcy
  (`WebhookResult`, `Handlers.WebhookReply`) — Slack weryfikuje URL echem `challenge`. Host przekazuje
  pluginom `GATOR_OUTBOUND_ALLOW_PRIVATE`, a `slack` i `github` wychodzą przez `netguard` (adres API
  to ustawienie). Nazwa pluginu `a2a` jest zarezerwowana (provider klientów A2A).
- **Slack**: kanały są mapowane w ustawieniu `channels` (`C0123=eng`), a nie przez `origin.slack_channel`;
  podpis v0 z oknem 5 minut, a replay w oknie łapie deduplikacja core'a po hashu body (Slack nie podaje
  id dostawy; ponowienie ma to samo body) i idempotencja `message.post` po `ts`. Workspace bota z
  `auth.test`; zdarzenie, komenda i przycisk innego workspace'u są odrzucane. `/gator ask` publikuje
  pytanie jako bot (żeby odpowiedź miała wątek). Wychodzące słowa są escapowane (`<!channel>` nikogo nie
  woła), bez unfurla. Teams i Telegram nie weszły (plan: po walidacji Slacka).
- **GitHub**: komentarz bez taska idzie do `#github` (agent odpowiada w wątku pod nim, co otwiera aska
  jak każda rozmowa), a nie od razu do nowego aska. Odpowiedź wraca jednym komentarzem, gdy jest
  końcowa, z wyłączonymi `@` i słowami zamykającymi; plugin pomija własne komentarze (znacznik) i boty.
  **Nie zrobione**: review agenta jako GitHub review z komentarzami w liniach (`review_comments` w
  digeście) — digest nie ma jeszcze takiego pola, a format trzeba ustalić z rolą reviewera.
- **ACP**: Gator nie oferuje agentowi `fs/*` ani `terminal/*` (agent używa własnych narzędzi), a
  `session/request_permission` odpowiada wg tieru. Job read-only (`chat`) idzie tylko na agenta, którego
profil zna tryb read-only, a sesja musi go oferować (inaczej start się nie udaje): nie każde narzędzie
agenta pyta przed działaniem, więc same odpowiedzi na prośby nie są obietnicą. Dziś to `codex`
(`read-only` codex-acp, do sprawdzenia); `gemini` bierze tylko joby `work`. Steer to `session/cancel`, potem nowy proces i
  `session/load` tej samej sesji; agent bez `loadSession` nie oddaje id sesji, więc executor zaczyna od
  nowa z poprawką. Backend ACP nie obsługuje `NoTools` (agent ma własne narzędzia, nie o wszystkie
  pyta), więc joby `summary` (streszczenia, kuracja) zostają na Claude. Koszt: z `usage_update`
  (`cost` w USD), tokeny z `usage` odpowiedzi; bez nich `costEstimated` i $0 — tabeli cen modeli innych
  dostawców nie ma. Profile (`codex`, `gemini`) to zmienne i pliki logowania (`AuthState`) i tryb
  read-only; nagrania w `testdata/<agent>` są **syntetyczne** (odegrane przez fałszywego agenta z
  binarki testów), bo adapterów (`codex-acp`, `gemini --experimental-acp`) nie było na maszynie, a sesja
  modelu jest płatna; jest tylko zapis `codex --version` i `codex login --help` (0.171.0). Runner buduje
  backendy z tabeli (`cmd/gator-runner/backends.go`), a nie ze switcha. Polityka i profil agenta
  przyjmują `claude` albo `acp:<nazwa>` (sprawdzane przy zapisie); ekran wyboru backendu to faza 6.
- **A2A**: cienka warstwa na JSON-RPC zamiast SDK, A2A 0.3 (`/.well-known/agent-card.json`,
  `message/send`, `tasks/get`; bez streamingu i push). Agent zdalny: `agents.kind`, `remote` (to, co z
  karty), `remote_secrets` (bearer zaszyfrowany keyringiem, nigdy w API — tylko `hasCredential`),
  CHECK: projekt, `chat`, bez roli, nieeksponowany. Turn agenta zdalnego nie jest jobem: Starter oddaje
  go temu, kto odpowiada za ten rodzaj agenta (`AnswerElsewhere`), a worker w serwerze robi wywołanie
  (`a2a_calls`, zajęcie z wygaśnięciem) i wpisuje odpowiedź do wiadomości agenta. Agent zdalny odpowiada
  tylko w rozmowach otwartych dla projektu i dostaje pytanie z ostatnimi 10 wiadomościami; jego słowa
  idą do innych agentów jako untrusted. Koszt turnu zdalnego to $0 (A2A go nie raportuje); limity hop,
  szerokości i budżetu obowiązują. Inbound: `PUT /agents/{id}/a2a` (eksponowanie), `a2a_clients` z
  `identity_id` (tożsamość `a2a`), `per_minute` i `revoked_at`, kwoty jako `double precision` jak
  `usage_records`; `a2a_tasks` łączy id taska A2A z wiadomością i agentem. Token `gtr_a2a_…`
  (nowy rodzaj tokenu) sięga tylko `/a2a/…`, a karta jest publiczna tylko dla agenta eksponowanego.
  Budżet dzienny liczy koszt jobów turnów zamówionych przez klienta (w toku po ich limicie). `tasks/cancel`
  odpowiada -32002 (turn zatrzymuje człowiek w Gatorze). Endpointy A2A są w `docs/openapi.yaml` (z
  `servers: /`), wyłączone z generowania (`exclude-operation-ids`).
- **Do sprawdzenia na żywo** (notki w `README.md` i `plugins/slack/README.md`): flagi i wersje adapterów
  ACP, raportowanie usage, id trybów; OAuth Slacka przez wspólny connect, zakresy, JSON w
  `chat.*`, ponowienia Slacka; wersje A2A i ścieżki kart u realnych peerów, ich odpowiedzi
  (`task` czy `message`) i koszt.
- **Punkt 3 (UI wyboru backendu w profilu agenta)** należy do fazy 6; `docs/openapi.yaml` opisuje
  wszystko, czego potrzebuje (`AgentPolicy.backend`, agent zdalny, eksponowanie, klienci A2A, kody
  powiązań i tożsamości z zewnątrz).
- **Po przeglądzie** (szczegóły w commicie): komenda dozwolona w read-only ACP to sama nazwa z PATH
  (`./gator-cli` z repozytorium jest odrzucane), a dopuszczenie narzędzia MCP po tytule dotyczy tylko
  wywołań bez rodzaju albo `other`; łańcuch delegacji zaczęty przez kogoś z zewnątrz zostaje w `chat`
  na każdym hopie (`Request.FromOutside`, dispatcher idzie po `parent_turn_id`), a budżet klienta A2A
  liczy cały łańcuch; każda tożsamość z zewnątrz ma dzienny budżet (strażnik `OutsideBudget`, $2 albo
  budżet klienta A2A); plugin bez własnego nagłówka dostawy (Slack) jest deduplikowany tylko po hashu
  body (`X-Gator-Delivery` nie robi z replayu nowej dostawy); Approve ze Slacka działa tylko, gdy task
  wciąż czeka na tę samą bramkę (klucz karty = faza i chwila wejścia); podpis odpowiedzi w GitHubie
  nie woła `@handle`; karta A2A musi wskazywać endpoint na swoim hoście (tam idzie token), a ten sam
  `messageId` do innego agenta to inne pytanie.
- **Po drugim przeglądzie** (szczegóły w commitach):
  - ACP: agent, który w jobie read-only sam zmieni tryb (`current_mode_update`), jest przerywany, a job
    kończy się porażką; `AuthState` patrzy tylko na zmienne i pliki logowania (nie na `CODEX_HOME`,
    `OPENAI_BASE_URL`); bufor słów agenta jest ograniczony (64 KiB niedokończonego akapitu, 1 MiB całości).
  - **Routing**: runner mówi, które backendy utrzymują job read-only, a które job bez narzędzi
    (`proto.Capabilities.read_only`, `no_tools`; runner sprzed tych pól — wszystkie), a serwer leasuje
    job `chat` i `summary` tylko tam. Job `summary` (streszczenia, kuracja) idzie na
    `GATOR_DEFAULT_BACKEND` serwera, cokolwiek polityka roli wybrała. Job `chat` z backendem, który
    nie umie read-only (np. `acp:gemini`), czeka na runner, który umie, i jest widoczny jako taki, który
    nie ma runnera — bez cichej zamiany.
  - **A2A**: Stop kończy biegnący turn agenta zdalnego (`Control` przez `Elsewhere.Stopped`), a
    wywołanie jest kończone; worker sprawdza turn przed wysłaniem, między pytaniami o task i przed
    zapisem. Worker robi 4 wywołania naraz, najwyżej 2 do jednego agenta, wywołanie próbowane 3 razy
    kończy się porażką, a agent zdalny odpowiada najwyżej na 100 wywołań dziennie (jego turn kosztuje
    $0, więc limit jest w wywołaniach). Wysyłki klienta są szeregowane od sprawdzenia budżetu do posta
    (na tym serwerze), najwyżej 5 jego pytań może być otwartych naraz, a `tasks/get` ma własny limit.
  - **Mosty**: tożsamość z zewnątrz pyta agentów najwyżej 6 razy na minutę (ponowienie dostawy to nie
    nowe pytanie); komentarz w GitHubie pyta tylko od OWNER, MEMBER, COLLABORATOR (ustawienie
    `commenters`; odpowiedź jest publiczna); `message.update` zmienia tylko wiadomość autora;
    Approve ze Slacka to `process.ApproveWaiting` (faza i chwila wejścia sprawdzane pod blokadą taska);
    kody powiązań są tylko dla mostu projektu, który je przyjmuje (`links` w manifeście: Slack tak,
    GitHub nie); `api_url` pluginu (tam idzie jego sekret) tylko na hoście pluginu albo na hoście z
    `GATOR_PLUGIN_API_HOSTS` operatora (`plugin/netguard.APIBase`). Polityka wyjścia przeniosła się do
    publicznego `plugin/netguard`, a `internal/server/netguard` jest jej fasadą.
  - Hooki zdarzeń (mosty) mają własny kursor outboxu: wolne API czatu nie wstrzymuje hooków innych
    pluginów. Sprzątacz usuwa przeterminowane kody, a host zapomina dostawy webhooków po 30 dniach
    (podpisana dostawa GitHuba starsza niż to byłaby obsłużona znowu; to, co z niej powstaje, i tak
    powstaje raz po id).
  - **00048 poprawiona w miejscu** (gałąź nie była wypchnięta): indeksy turnów z zewnątrz, turnów po
    rodzicu i mostów po rozmowie. W `docs/openapi.yaml`: `requestedByKind` ma `external`,
    `Agent.kind` jest enumem, `Message.origin` jest opisany.
  - **Zawężone**: szeregowanie wysyłek klienta A2A i limity na minutę są w pamięci jednego serwera
    (jak limity webhooków); przy kilku węzłach budżet i liczba otwartych pytań i tak trzymają.


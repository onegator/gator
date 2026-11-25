# Faza 4: Marketplace

**Cel:** zespół w kilka kliknięć dodaje do projektu agenta z rolą (np. „QA Engineer"), skill (np.
„pisanie migracji goose"), integrację (natywny plugin albo zdalny serwer MCP) czy szablon procesu.
Wszystko przychodzi z rejestru, ma wersję, checksum, wymagania i jawną listę uprawnień do
zaakceptowania.

**Poza zakresem:** płatności i sprzedaż paczek, otwarta publikacja przez społeczność (najpierw
rejestr kurowany przez nas, patrz założenia w [README](README.md)).

## Rodzaje paczek

| Kind | Zawartość | Instalacja tworzy |
|---|---|---|
| `agent` | `agent.yaml`, `instructions.md`, opcjonalnie `skills/`, wymagane integracje, automations | wiersz `agents` z `source.kind='marketplace'` |
| `skill` | folder w formacie Agent Skills: `SKILL.md` (frontmatter `name`, `description`, opcjonalnie `example-prompt`), `scripts/`, `references/`, `assets/icon.png` | wiersz `skills`, przypinany do agentów |
| `integration` | natywny plugin (binarka per platforma + manifest) **albo** definicja zdalnego serwera MCP | `plugins` / `mcp_servers` |
| `process` | szablon kindu (`*.yaml` jak `internal/server/process/defaults`) i role | wpis w `process_config` projektu lub workspace'u |
| `knowledge` | dzisiejszy knowledge pack | `knowledge_packs` (bez zmian) |

## Format rejestru

Rozszerzamy to, co już działa dla knowledge packs (`knowledge/index.json`: nazwa, wersja, ścieżka,
`sha256` całej paczki; paczka z niezgodnym checksumem jest odrzucana). Rejestr to repozytorium git:

```
index.json                 {"items":[{"kind":"agent","name":"qa-engineer","version":"1.2.0","path":"agents/qa-engineer","checksum":"sha256:…"}]}
agents/qa-engineer/
  agent.yaml
  instructions.md
  skills/e2e-playwright/SKILL.md
  icon.png
skills/goose-migrations/SKILL.md
integrations/sentry-mcp/integration.yaml
process/incident-postmortem/process.yaml
```

Podpis: `index.json.sig` (minisign/cosign, klucz rejestru w konfiguracji serwera). Rejestr Gatora
jest wbudowany jako domyślny (`GATOR_MARKETPLACE_URL`), a własne rejestry firmy dodaje admin
workspace'u. Komenda `gator-server marketplace checksum <dir>` działa jak dziś
`knowledge checksum`.

### `agent.yaml`

```yaml
name: qa-engineer
version: 1.2.0
display_name: QA Engineer
handle: qa                         # sugerowany; konflikt = admin wybiera inny
description: Pisze i utrzymuje testy e2e, sprawdza kryteria akceptacji przed release.
avatar: checkmark.seal
phases: [verification, release]    # gdzie w SDLC (do filtrów i rekomendacji)
role_key: tester                   # może przejąć fazę o tej roli w szablonie
tier: work
policy: {model: claude-sonnet-5, max_cost_usd: 3}
skills: [e2e-playwright]
requires:
  capabilities: [vcs, ci]          # jak w pluginach: o możliwości, nie o nazwę
  mcp: []                          # opcjonalne zdalne MCP
triggers:                          # proponowane automations (każdą i tak włącza człowiek)
  - on: pull_request
    when: {labels: [needs-qa]}
permissions:                       # pokazywane przy instalacji, do zaakceptowania
  - writes code on gator/* branches
  - reads CI logs
```

### `integration.yaml` dla zdalnego MCP

```yaml
name: sentry-mcp
kind: mcp
url: https://mcp.sentry.dev/mcp
auth: oauth                       # oauth | bearer | none
capabilities: [monitoring]
tools_default_allow: [search_issues, get_issue_details]   # reszta wyłączona, dopóki admin nie włączy
```

## Integracje: dwa tory

1. **Natywne pluginy** (dziś: GitHub, Linear, Sentry, Honeybadger). To głęboka integracja z procesem:
   webhooki, `gateEvaluate`, `turnEnding`, import. Zostają naszą przewagą i są dystrybuowane przez
   marketplace (binarka per `GOOS/GOARCH`, checksum, `GATOR_PLUGIN_DIR`).
2. **Zdalne serwery MCP**, czyli narzędzia dla agentów (Notion, Figma, Datadog, Stripe, Atlassian…).
   Setki integracji bez pisania pluginów.

### Proxy MCP: sekrety zostają na serwerze

Runner **nigdy** nie dostaje tokenu do zewnętrznego MCP. `--mcp-config` joba wskazuje
`https://<gator>/mcp/proxy/<server>` z tokenem joba, a gator-server:
- sprawdza, czy agent joba ma ten serwer w `tools` i czy narzędzie jest na liście dozwolonych;
- wstrzykuje poświadczenia (OAuth przez istniejący `connect/callback`, sekrety szyfrowane jak
  `project_plugins.secrets`);
- zapisuje każde wywołanie (jak `plugin_calls`: metoda, czas, błąd, payload zredagowany);
- odpowiedź z zewnątrz traktuje jak dane: limit rozmiaru i znaczniki untrusted przy oddawaniu do
  kontekstu;
- działa w `limits.Breaker`, jak pluginy.

```sql
-- 00047_mcp_servers.sql
CREATE TABLE mcp_servers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid REFERENCES projects(id) ON DELETE CASCADE,   -- NULL = workspace
    name        text NOT NULL,
    url         text NOT NULL,
    auth        text NOT NULL CHECK (auth IN ('oauth','bearer','none')),
    secrets     text NOT NULL DEFAULT '',          -- zaszyfrowane keyringiem serwera
    allowed_tools text[] NOT NULL DEFAULT '{}',
    enabled     boolean NOT NULL DEFAULT true,
    source      jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);
CREATE TABLE mcp_calls ( … jak plugin_calls, append-only … );
```

## Skille na runnerze

Skill trafia do joba w trzech trybach, zależnie od backendu (ten sam podział co w OpenBocie, ale to
nasza implementacja):
- `native`: backend czyta katalog skilli sam (Claude Code). Runner kładzie skille w katalogu
  **poza worktree** (żeby nie brudzić gita) i podaje go backendowi. Mechanizm do potwierdzenia na
  aktualnym CLI (plugin dir / add-dir), z nagraniem w `testdata`;
- `instructions`: backend nie zna skilli (część ACP, faza 5). Runner dokleja do promptu listę
  `name: description`, a treść skill agent czyta przez `skill_read` w MCP;
- skrypty skilla nie są uruchamiane przy instalacji, tylko przez agenta w trakcie joba, w jego
  uprawnieniach (`tier`).

## Dane (`00046_marketplace.sql`)

```sql
CREATE TABLE marketplace_registries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL UNIQUE, url text NOT NULL,
    public_key text NOT NULL DEFAULT '',       -- weryfikacja podpisu indeksu
    builtin boolean NOT NULL DEFAULT false,
    fetched_at timestamptz
);
CREATE TABLE marketplace_items (                -- cache indeksu
    registry_id uuid NOT NULL REFERENCES marketplace_registries(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('agent','skill','integration','process','knowledge')),
    name text NOT NULL, version text NOT NULL,
    manifest jsonb NOT NULL, checksum text NOT NULL,
    files jsonb NOT NULL DEFAULT '[]',
    PRIMARY KEY (registry_id, kind, name, version)
);
CREATE TABLE installations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid REFERENCES projects(id) ON DELETE CASCADE,   -- NULL = workspace
    registry_id uuid NOT NULL REFERENCES marketplace_registries(id),
    kind text NOT NULL, name text NOT NULL, version text NOT NULL,
    target_id uuid,                     -- agents.id / skills.id / mcp_servers.id …
    accepted_permissions jsonb NOT NULL,-- dokładnie to, co człowiek zaakceptował
    installed_by uuid NOT NULL REFERENCES users(id),
    installed_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, kind, name)
);
CREATE TABLE skills (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid REFERENCES projects(id) ON DELETE CASCADE,
    name text NOT NULL, version text NOT NULL, description text NOT NULL,
    files jsonb NOT NULL,                -- cache całego folderu; runner dostaje go w leasie
    source jsonb NOT NULL,
    UNIQUE (project_id, name)
);
```

**Aktualizacje:** nowa wersja w rejestrze pokazuje chip „Update" i nie aktualizuje sama. Jeśli nowa
wersja prosi o **więcej uprawnień** (`permissions`, `requires`, nowe narzędzia MCP), wymaga ponownej
akceptacji. Lokalne zmiany w zainstalowanym agencie (edycja instrukcji) blokują automatyczny merge i
pokazują diff.

## Katalog startowy (rejestr Gatora)

Agenci (domyślni z fazy 2 są już w projekcie, a te poniżej doinstalowuje się z katalogu):

| Agent | Faza SDLC | Co robi | Wymaga |
|---|---|---|---|
| Discovery Researcher | discovery | rynek, konkurencja, wywiady → brief | web |
| Product Spec Writer | idea → discovery | PRD, kryteria akceptacji, persony do Brainu | tracker |
| Architect | planning | ADR, wpływ na komponenty, aktualizacja katalogu | vcs |
| Implementer (`@worker`) | implementation | kod na gałęzi | vcs |
| Reviewer | verification | review PR, zgodność z konwencjami z Brainu | vcs |
| Security Reviewer | verification | threat model, sekrety, zależności | vcs |
| QA Engineer | verification / release | testy e2e, scenariusze z kryteriów | vcs, ci |
| Release Manager | release | changelog, release notes, checklisty | deploy |
| SRE / On-call Triage | monitoring | triage błędów i incydentów, runbooki do Brainu | monitoring |
| Docs Writer | po release | dokumentacja z diffów i decyzji | vcs |
| Weekly Reporter | cały cykl | podsumowanie tygodnia, metryki, ryzyka | — |
| Librarian (`@curator`) | cały cykl | utrzymanie Brainu: duplikaty, sprzeczności, przeterminowane | — |

Skille startowe: `goose-migrations`, `openapi-first`, `go-testing`, `swiftui-snapshots`,
`incident-postmortem`, `adr-writing`, `release-notes`.

Integracje startowe: natywne GitHub, Linear, Sentry, Honeybadger (już są) oraz MCP dla Atlassian,
Notion, Figma, Datadog, Grafana i Slack (narzędzia, nie most; most jest w fazie 5).

## API

```
GET  /marketplace?kind=&q=&phase=                 katalog ze wszystkich rejestrów (+ zainstalowane)
GET  /marketplace/{kind}/{name}                   szczegóły, wersje, uprawnienia, README, przykładowy prompt
POST /projects/{projectId}/installations          {kind, name, version, accepted_permissions}
PATCH /installations/{id}                         update do wersji (z ponowną akceptacją, gdy trzeba)
DELETE /installations/{id}
GET  /registries  POST /registries  DELETE /registries/{id}     (admin workspace'u)
GET  /projects/{projectId}/mcp-servers  PUT …/{name}  POST …/{name}/connect
```

## Podział na PR-y

1. Format rejestru, checksum i podpis, `gator-server marketplace checksum`, fetch do `marketplace_items`.
2. Instalacja `agent` (tworzy `agents`) z akceptacją uprawnień.
3. `skills`: dostarczanie do joba w leasie (proto), tryby `native` / `instructions`, `skill_read` w MCP.
4. `mcp_servers` i proxy `/mcp/proxy/<server>` z allowlistą, audytem i breakerem.
5. OAuth dla zdalnych MCP przez istniejący `connect/callback`.
6. Dystrybucja natywnych pluginów z rejestru (binarka per platforma, checksum).
7. `process`: instalacja szablonu kindu.
8. Aktualizacje i diff uprawnień.
9. Rejestr Gatora: osobne repo `onegator/gator-marketplace` z katalogiem startowym i CI liczącym
   checksumy (zamiast dzisiejszego `knowledge/` w repo; `knowledge/` przenosimy tam).
10. Ekran Marketplace (faza 6b).

## Gotowe, gdy

- Admin projektu instaluje „QA Engineer", akceptuje uprawnienia i `@qa` od razu jest w @autocomplete.
- Agent z dostępem do Sentry MCP przez proxy czyta issue, a token Sentry nie pojawia się na runnerze
  (test: zrzut env i plików joba).
- Paczka ze zmienionym plikiem po publikacji jest odrzucana.

## Ryzyka

- **Łańcuch dostaw.** Paczka to prompt i skrypty, które agent uruchomi z pełnymi uprawnieniami
  runnera. Stąd na start rejestr kurowany, podpisy, akceptacja uprawnień i brak auto-update. Otwarcie
  na społeczność wymaga review i automatycznego skanu (np. sekrety, sieć w skryptach).
- **Prompt injection przez narzędzia MCP.** Odpowiedź z zewnętrznego MCP może zawierać instrukcje.
  Pomaga allowlista narzędzi, znaczniki untrusted i to, że agent `chat` nie ma narzędzi zapisu.

## Odstępstwa od planu (zapisane przy implementacji)

- **Migracje**: `00046_marketplace.sql` (rejestry, cache paczek, instalacje, skille) i
  `00047_mcp_servers.sql` (serwery MCP, `mcp_calls` i kolumna `target` w `plugin_connect_states`,
  dopisana w miejscu w kroku 5 — gałąź nie była wypchnięta). 00045 dalej czeka na embeddingi (notka o
  `goose allow-missing` w `README.md`), fazy 5 zostają przy 00048–00049. `marketplace_items` ma też
  `listed` (czy ostatni fetch jeszcze ją wymienia), `fetched_at`; rejestr — `fetch_error`,
  `created_by`; instalacja — `checksum`, `installed` (manifest i to, co człowiek może potem zmienić),
  `updated_at`; `UNIQUE NULLS NOT DISTINCT`, żeby workspace (NULL) też miał jedną instalację na paczkę.
  `store/marketplace_migrations_test.go` robi na osobnej bazie: wejście na 00044 z danymi, up, dane
  rynku, down do 00044 i znów up, sprawdzając, że nic sprzed rynku nie znika.
- **Podpis**: zwykły Ed25519 nad bajtami `index.json` (biblioteka standardowa), klucz
  `ed25519:<base64>`, podpis w `index.json.sig` jako base64. Minisign i cosign dają więcej (komentarze
  zaufane, przezroczystość), ale kurowany rejestr z jednym kluczem tego nie potrzebuje, a własny format
  to zero zależności i podpisywanie w testach bez narzędzi z zewnątrz. Rotacja: serwer przyjmuje kilka
  kluczy po przecinku. Rejestr firmy bez klucza jest „unsigned” (pole `signed` w API) i trzymają go
  tylko checksumy. Klucz prywatny rejestru Gatora leży poza oboma repozytoriami
  (`~/.config/gator-marketplace/registry.key` u maintainera); publiczny jest domyślnym
  `GATOR_MARKETPLACE_PUBLIC_KEY`.
- **Checksum** jest z samych plików paczki (ścieżka, długość, bajty, w kolejności ścieżek), nie z
  manifestu jak w knowledge packs; indeks to `{"items": [...]}`, jeden wpis na linię. Stary `/packs`
  (`knowledge.Fetch`) dalej czyta indeksy z `packs` dla starych rejestrów; paczki wiedzy z rejestru
  instaluje się przez marketplace (`knowledge` → `knowledge_packs` i włączenie w projekcie).
  **Opublikowana wersja się nie zmienia**: fetch, w którym ta sama wersja ma inne pliki, jest odrzucany
  (to jest test „paczka zmieniona po publikacji”), obok zwykłego niezgodnego checksumu.
- **Fetcher / Verifier / Kind / Installer** są osobno: `Git` (klon bez hooków i submodułów) i `Dir`
  (`file://` albo ścieżka — tylko admin workspace'u dodaje rejestr), `Verifier` = podpis indeksu +
  checksum i limity paczki (200 plików, 1 MiB na plik, 4 MiB razem, indeks 1 MiB; bez linków
  symbolicznych, ścieżki tylko w głąb), `Kind` czyta manifest i wylicza uprawnienia, `Installer`
  instaluje w transakcji razem z wierszem instalacji. Integracje mają tory (`Track`: `mcp`, `native`).
  Rejestr Gatora jest pobierany przy starcie serwera (w tle), dalej na żądanie
  (`POST /registries/{id}/fetch`); nie ma okresowego odświeżania.
- **Uprawnienia** to linie: jawne z manifestu i wyliczone (tier `work`, limit kosztu, rola procesu,
  `requires`, serwery i narzędzia MCP, proponowane automations, skrypty i `allowed-tools` skilli, host i
  logowanie zdalnego MCP, platformy natywnego pluginu, kind i role procesu). Instalacja wymaga każdej
  (porównanie po znormalizowanych spacjach), a zapisuje dokładnie zaakceptowane. Wymagania, których
  projekt nie ma (capability, skill, serwer MCP), są pokazywane (`missing`), ale nie blokują.
- **Agent**: uchwyt z manifestu albo `handle` z żądania (zajęty = 409); `agents.tools` = `tools` ∪
  `requires.mcp`; `triggers` są tylko uprawnieniem do zaakceptowania — automations i tak włącza
  człowiek, nic ich nie tworzy. Agenta z rynku usuwa się przez jego instalację (`DELETE /agents/{id}` →
  409). Uchwyty katalogu nie kolidują z domyślnymi (`@implementer`, `@code-reviewer`, `@appsec`,
  `@oncall`, `@librarian`… zamiast `@worker`, `@reviewer`, `@curator`); agent z `role_key` przejmuje
  rolę w projekcie, co jest jego uprawnieniem.
- **Skille**: tabela `skills` (projekt albo workspace); skill przyniesiony przez agenta ma
  `source.agent` i znika z agentem, gdy nikt inny go nie nosi; `skills`/`tools` agenta zmienia
  `PATCH /agents/{id}`. Lease niesie skille agenta (`proto.Job.Skills`, do 2 MiB) i nazwy serwerów MCP
  (`proto.Job.MCP`) — pola dodane do v3 bez podbijania protokołu, bo starszy runner po prostu ich nie
  użyje. Tryb `native`: Claude Code 2.1.332 ładuje skille z `--plugin-dir` (plugin `gator-skills` w
  katalogu runnera 0700 poza worktree, usuwany po runie; układ potwierdzony `claude plugin validate`,
  nagranie w `testdata/claude-2.1.332-plugin-validate.json`, bez płatnej sesji); sesja read-only
  dostaje narzędzie `Skill`, skrypty mają 0700 i uruchamia je tylko Bash tam, gdzie wolno. Tryb
  `instructions`: blok „Skills you have” na końcu promptu i `skill_read` w MCP (jobscope: faza i
  reply); bez tożsamości joba `SKILL.md` idzie wprost do promptu.
- **Serwery MCP i proxy** (`internal/server/mcpremote`): także serwery workspace'u
  (`/workspace/mcp-servers`); nazwa `gator` zarezerwowana; serwer można zrobić ręcznie (`PUT`, tylko
  `bearer`/`none`). Proxy to bezstanowy serwer go-sdk na żądanie (`POST /mcp/proxy/<name>`, jobscope
  `mcp proxy`) i klient go-sdk do serwera, poskładane z kawałków: `Gate`, `Allowlist`, `Credentials`,
  `Recorder`, `Breakers`, fence. Agent `chat` dostaje tylko narzędzia z `readOnlyHint` (plan: chat nie ma
  narzędzi zapisu). Odpowiedź wraca jako jeden tekst w znacznikach untrusted (do 64 KiB); obrazy i
  `structuredContent` nie wracają, a opis narzędzia ma prefiks serwera i limit długości. Breaker per
  serwer w pamięci (5 porażek / minuta; liczy się serwer nieosiągalny, nie odpowiedź JSON-RPC typu „złe
  argumenty”), zerowany przy `PUT`. Lista narzędzi w cache na minutę. `mcp_calls` jest append-only poza
  kaskadami (`pg_trigger_depth`). Test „token nigdy na runnerze” uruchamia prawdziwy executor i backend
  Claude z nagraną sesją i skryptem, który zrzuca env, argumenty i pliki joba; w trakcie joba test woła
  proxy z konfiguracji MCP sesji, tak jak zrobiłby to Claude Code.
- **OAuth MCP** (po review): zgodnie ze specyfikacją autoryzacji MCP — protected resource metadata
  (RFC 9728) serwera, metadata serwera autoryzacji (RFC 8414), dynamiczna rejestracja klienta (RFC
  7591) per serwer (klient w zaszyfrowanych sekretach), PKCE S256 i `resource` (RFC 8707). Paczka nie
  podaje endpointów, tylko serwer autoryzacji, który musi się zgadzać z tym z metadanych (domyślnie
  origin serwera MCP), a jego endpointy muszą leżeć na jego hoście. Aplikacja serwera per dostawca
  (`GATOR_<PROVIDER>_CLIENT_ID`) jest używana tylko na hostach z `GATOR_<PROVIDER>_OAUTH_HOSTS`.
  Callback jest wspólny z pluginami: stan ma `target`, a plugins dostają `Connector`. Token odnawia się
  przy użyciu, z blokadą per serwer; tokenów i klienta OAuth nie da się wkleić. Łączenie tylko dla
  serwerów projektu.
- **Natywne pluginy**: tylko dla workspace'u (pluginy są workspace'owe, projekt je włącza). Build dla
  platformy serwera, pobrany (https, http do localhost albo `file://`) do pliku 0600 z liczeniem sha256,
  wykonywalny dopiero po zgodności, w `GATOR_PLUGIN_DIR/marketplace/<name>/<version>/<name>`.
  `Host.Install` działa poza transakcją instalacji. Odinstalowanie tylko, gdy żaden projekt go nie ma
  (`plugin_calls` jest append-only). W rejestrze nie ma wpisów natywnych: GitHub, Linear, Sentry i
  Honeybadger dalej są w paczce serwera, a wpis przyjdzie z wydaniem buildów.
- **Proces**: zapisuje `templates.<kind>` i `roles.<rola>` w `process_config` (projektu albo
  workspace'u), resztę konfiguracji zostawia; to, co zastąpił, oddaje przy usunięciu.
- **Aktualizacje**: `GET /installations/{id}/update` (podgląd: dodane i usunięte uprawnienia, zmiany
  lokalne jako diffy) i `PATCH /installations/{id}` (`acceptedPermissions`, `resolve: keep_local |
  take_theirs`). Instalator, który umie aktualizować w miejscu, to `Updater`; części porównywane z
  „jak zainstalowano”: instrukcje agenta, `SKILL.md` skilla, szablon procesu. Agent zachowuje uchwyt,
  serwer MCP allowlistę (dochodzą nowe domyślne narzędzia, właśnie zaakceptowane) i — przy tym samym
  adresie — poświadczenia; natywny plugin dostaje nowy build sprawdzony jak pierwszy.
- **Punkt 9**: rejestr jest lokalnym repozytorium `gator-marketplace` (bez remote): 12 agentów, 7
  skilli (+ `e2e-playwright` w QA Engineer), 7 serwerów MCP, proces `incident-postmortem` i paczka
  `go-service` przeniesiona z `knowledge/` (usuniętego z gatora). Makefile i CI wołają
  `gator-server marketplace index|verify|sign` z checkoutu gatora, więc checksum liczy ten sam kod, który
  go sprawdza. Testy gatora budują rejestr startowy w katalogu testu (`marketplacetest`) i wskazują
  wbudowany rejestr na `file://`. Nazwy narzędzi i endpointy OAuth serwerów MCP są „na dziś” — admin
  widzi prawdziwe narzędzia w `GET …/mcp-servers/{name}/tools`.
- **Punkt 10**: ekran Marketplace należy do fazy 6b; `docs/openapi.yaml` opisuje wszystko, czego
  potrzebuje: `/marketplace`, `/marketplace/{kind}/{name}` (wersje, uprawnienia, README, przykładowy
  prompt, `missing`, `installed`), `/registries`, instalacje projektu i workspace'u, podgląd i
  aktualizacja, `/projects/{id}/skills`, serwery MCP (lista, ustawienia, narzędzia, wywołania, connect).
- **Po review** (szczegóły w commitach): połączenie (plugin i MCP) zaczyna się na `/connect/start`,
  które wiąże je z przeglądarką ciasteczkiem, a callback przyjmuje tylko tę przeglądarkę (i tę samą
  osobę, gdy przeglądarka jest zalogowana do Gatora); paczka natywna nie zajmie nazwy pluginu, którego
  nie zainstalowała (a nieudana instalacja sprząta wiersz i binarkę); zmiana adresu serwera MCP i
  usunięcie jego sekretów to jeden UPDATE pod blokadą wiersza, a rozpoczęte logowania znikają; wyjście
  serwera (MCP, OAuth, pobrania, rejestry) idzie przez `netguard` (żadnych adresów prywatnych, link-local,
  metadata ani plain http, sprawdzane przy łączeniu; `GATOR_OUTBOUND_ALLOW_PRIVATE=1` dla sieci firmy,
  `GATOR_MARKETPLACE_ALLOW_FILE=1` dla `file://`); narzędzia read-only dla chat to migawka z chwili
  dopuszczenia (`read_only_tools`, z paczki `tools_read_only`), a grant jest chat, gdy chat jest job albo
  agent; odpowiedzi serwera do 4 MiB, lista do 256 narzędzi, schemat do 16 KiB, tytuł jedna linia; koszt,
  budżet dzienny i model zawsze są linią uprawnień; lokalne zmiany agenta to też tools, skills, policy,
  tier i rola, a skille porzucone przez nową wersję znikają; proces ma jednego właściciela kindu i roli w
  zakresie, a usunięcie oddaje tylko to, co wciąż jest jego; instalacja i aktualizacja podają rejestr,
  wersję i checksum (rejestr Gatora pierwszy przy kolizji nazw), a aktualizacja jest liczona i
  stosowana w jednej transakcji na zablokowanej instalacji, bez cichego cofania wersji; indeks ma
  `serial` i `expires` (odrzucany replay, fork serialu i zamrożenie), paczki czytane przez `os.Root`,
  link w ścieżce paczki odrzucany, odmowa nie podaje hasha; rejestr bez klucza tylko przez https/ssh i
  pokazany jako `trusted: false` z linią uprawnień; adres rejestru i błąd fetcha tylko dla adminów, bez
  poświadczeń; wywołania serwerów workspace'u czyta admin workspace'u, argumenty maskowane też po
  kształcie sekretu, a adres MCP nie może nieść poświadczeń w query; breaker nie liczy serwera
  niepołączonego ani anulowanego wywołania; `mcp_calls` przeżywają serwer (po nazwie), a `migrate`
  dopuszcza brakujące 00045. Deadlock z testów: `Threads.ForTask` bierze teraz projekt i task przed
  kanałem, posty i wątki ponawiają transakcję przy 40P01, a pakiety z konsumentami outboxu (api,
  conversations, e2e) mają własne bazy (`storetest.Main`).
- **Nie zrobione**: okresowe odświeżanie rejestrów; automatyczny skan paczek (sekrety, sieć w
  skryptach) przed otwarciem na społeczność; wpisy natywnych pluginów w rejestrze; OAuth discovery MCP i
  łączenie serwerów workspace'u przez OAuth; `skill read` przez REST/gator-cli (jest tylko w MCP).

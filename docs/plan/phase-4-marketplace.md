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
| `process` | szablon kindu (`*.yaml` jak `process/defaults`) i role | wpis w `process_config` projektu lub workspace'u |
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
-- 00043_mcp_servers.sql
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

## Dane (`00042_marketplace.sql`)

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

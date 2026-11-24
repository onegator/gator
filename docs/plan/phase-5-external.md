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
-- 00044_external_agents.sql
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
-- 00045_messaging_bridges.sql
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

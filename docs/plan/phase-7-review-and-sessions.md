# Faza 7: Przegląd zmian i żywe sesje agentów

Stan na 2026-11-25. Źródło pomysłów: [pingdotgg/t3code](https://github.com/pingdotgg/t3code).

**Cel.** Człowiek widzi, co agent zmienił w każdej turze, komentuje konkretne linie, a komentarz
wraca do agenta jako zadanie. Agent pracuje w jednej żywej sesji: można go sterować w trakcie tury,
zatrzymać od razu, a ryzykowne narzędzie czeka na zgodę człowieka z karty w rozmowie.

Faza zamyka też trzy luki zostawione po fazie 6:
- załączniki nie docierają do agenta;
- widok taska nie odróżnia pracy w fazie od odpowiedzi i streszczeń;
- aplikacja nie wysyła sygnału „pisze…”.

**Poza zakresem:**
- agenci uruchamiani na komputerze członka zespołu (u nas zostają na runnerze);
- relay w stylu T3 Connect;
- osobne sterowniki dla każdego CLI (zostajemy przy natywnym Claude i ścisłym ACP);
- workflowy jako skrypty;
- przełączanie modelu w środku wątku (osobny temat na później).

## Licencja i co wolno wziąć

- t3code jest na licencji **MIT** (`LICENSE`, „Copyright (c) 2026 T3 Tools Inc.”). Kod wolno
  kopiować, także do produktu komercyjnego, jeśli w skopiowanych plikach zostaje nota copyright i
  tekst licencji.
- Nazwy „T3 Code”, logo i ikon nie wolno używać, nie wolno też sugerować afiliacji (`TRADEMARK.md`).
- Serwer t3code jest w TypeScript na frameworku Effect, więc z serwera i runnera bierzemy
  **wzorce, nie kod**.
- Jedyny kod, który realnie przenosimy, to natywny widok diffu w Swift
  (`apps/mobile/modules/t3-review-diff/ios/T3ReviewDiffView.swift`, `ReviewDiffCodeLayout.swift`).
  Przy przeniesieniu: nota MIT w nagłówku plików, `THIRD_PARTY_NOTICES.md` w gator-app, zmienione
  nazwy typów (bez „T3”) i usunięta zależność od `ExpoView`.

## Co już jest w Gatorze, a co się zmienia

| Dziś | Po fazie 7 |
|---|---|
| Steer przerywa proces i wznawia sesję przez `--resume` z nowym promptem (`runner/executor/executor.go`, `errSteered`, limit 20 steerów) | steer to kolejna wiadomość użytkownika wrzucona do żywej sesji; przerwanie zostaje tylko jako fallback dla backendów, które nie umieją inaczej |
| Stop ubija grupę procesów | stop to najpierw natywne `interrupt`, a ubicie procesu dopiero po limicie czasu |
| Tier `chat` / `work` i consent na tasku (00033, faza 2), bez zgód na pojedyncze narzędzia | tiery zostają; w `work` dochodzą zgody na żywo dla narzędzi spoza polityki (karta `tool_approval`) |
| Zmiany agenta widać jako artefakty i commity na gałęzi `gator/…` | diff każdej tury z checkpointów i komentarze na liniach |
| `Backend` ma tylko `Limited.Supports(Access)` | macierz możliwości backendu raportowana przez runner, z której aplikacja chowa niedostępne akcje |
| Testy parsera Claude na 14 nagraniach w `backend/claude/testdata` | ten sam wzorzec dla ACP i dla nowego trybu sesji; nagranie jest warunkiem wsparcia backendu |
| Załączniki trafiają na serwer (faza 1), ale do agenta nie | agent dostaje ścieżki do plików poza worktree |

## Kolejność i zależności

```
7.1 Możliwości + nagrania ──► 7.4 Żywa sesja Claude ──► 7.5 Zgody na narzędzia ──► 7.6 Steer czy kolejka
        │
        └──► 7.2 Checkpointy ──► 7.3 Diff w aplikacji + komentarze

7.7 Załączniki dla agenta   7.8 Domknięcia po fazie 6   7.9 Powiadomienia     (niezależne)
```

| # | Część | Gdzie | Wysiłek | Wartość |
|---|---|---|---|---|
| 7.1 | Macierz możliwości backendów i testy na nagraniach | runner, proto, serwer, aplikacja | S | fundament |
| 7.2 | Checkpoint po każdej turze na ukrytych refach, diff tury | runner, serwer | M | bardzo wysoka |
| 7.3 | Natywny widok diffu i komentarze na liniach dla agenta | aplikacja, serwer | M | wysoka |
| 7.4 | Claude jako żywa sesja ze strumieniowym wejściem | runner | M–L | wysoka |
| 7.5 | Zgody na narzędzia na żywo (karta `tool_approval`) | runner, serwer, aplikacja | M | wysoka |
| 7.6 | Steer czy kolejka przy wiadomości, kolejka zapisana na serwerze | serwer, aplikacja | S–M | średnia |
| 7.7 | Załączniki dla agenta, duża wklejka jako plik | serwer, runner, aplikacja | M | wysoka |
| 7.8 | Domknięcia po fazie 6 i stan „rozliczanie” po turze | serwer, aplikacja | S | średnia |
| 7.9 | Push i Live Activity: „czeka na zgodę”, „skończył” | serwer, aplikacja iOS | M | średnio-wysoka |

Szacunek: około 5–6 tygodni dla 1–2 osób z agentami. 7.1 idzie pierwsze, bo 7.4 i 7.5 opierają
się na macierzy możliwości. 7.2 i 7.3 można robić równolegle z 7.4.

---

## 7.1 Macierz możliwości backendów i testy na nagraniach

**Po co.** Dziś aplikacja i serwer zakładają, że każdy backend umie to samo co Claude. Backend ACP
z fazy 5 nie umie wszystkiego, a nowe funkcje (steer w trakcie tury, zgody, cofanie) jeszcze
bardziej się rozjadą. Wzór: `OrchestrationV2ProviderCapabilities` w
`packages/contracts/src/orchestrationV2.ts:301-448` w t3code.

**Proto** (`internal/proto`, wersja v4; serwer mówi v4 i v3):

```go
type Capabilities struct {
    ActiveSteering         bool   // wiadomość trafia do trwającej tury bez restartu
    SteeringInterruptsTool bool   // steer przerywa narzędzie w toku
    NativeInterrupt        bool   // stop bez ubijania procesu
    ToolApprovals          bool   // backend pyta o zgodę na narzędzie i czeka
    ApprovalsLiveOnly      bool   // zgody nie przeżyją restartu procesu
    StreamsText            bool   // delty tekstu, nie tylko gotowe wiadomości
    StreamsReasoning       bool
    Attachments            bool   // przyjmuje pliki (ścieżki lub obrazy) jako wejście
    Checkpoints            bool   // runner umie zrobić checkpoint w tym workspace
    Enforcement            string // "native" | "runner": kto pilnuje ograniczeń tieru
}
```

- `Backend` dostaje metodę `Capabilities() proto.Capabilities`.
- `register` i `heartbeat` raportują możliwości per backend.
- Serwer zapisuje je w `runners.capabilities` (jsonb już jest) i oddaje w
  `GET /runners/{id}` oraz w `Job.capabilities` (to, czym dysponuje runner, który wziął job).
- Aplikacja chowa Steer, Stop natywny, karty zgód i podobne akcje, gdy job ich nie obsłuży, zamiast
  pokazywać przycisk, który nie zadziała.

**Nagrania.**
- `backend/acp/testdata/<agent>-<wersja>-*.ndjson` dla każdego wspieranego agenta ACP.
- `backend/claude/testdata/claude-<wersja>-session-*.ndjson` dla nowego trybu sesji (7.4): steer w
  trakcie tury, interrupt, prośba o zgodę i odpowiedź, wznowienie.
- Wspólny helper `internal/runner/backend/replay` puszcza nagranie przez prawdziwy parser i porównuje
  eventy joba ze złotym plikiem. Wzór: `packages/provider-testing/src/replayTranscript.ts`.
- Zasada: backend bez nagrania nie trafia na listę wspieranych.

**Gotowe, gdy:** runner raportuje możliwości, a aplikacja nie pokazuje Steer dla backendu bez
`ActiveSteering` ani `--resume`. Nagrania ACP i Claude przechodzą w CI.

---

## 7.2 Checkpointy na ukrytych refach i diff tury

**Po co.** Dziś człowiek widzi wynik całego joba (artefakty, commity na gałęzi). Nie widzi, co
zmieniła *ta* tura, a commity agenta mieszają się z tym, co przyszło z rebase'u. Wzór:
`apps/server/src/vcs/GitVcsDriver.ts:1345-1948`, `orchestration-v2/CheckpointService.ts`,
`vcs/checkpointCaptureIndex.ts`.

**Runner.**
1. Przed pierwszą turą joba i po każdej turze robi checkpoint workspace'u, nie dotykając gałęzi ani
   indeksu agenta:
   ```
   GIT_INDEX_FILE=<tmp> git add -A
   GIT_INDEX_FILE=<tmp> git write-tree               → tree
   git commit-tree <tree> -p <poprzedni checkpoint>  → commit
   git update-ref refs/gator/checkpoints/<job>/<n> <commit>
   ```
2. Indeks z poprzedniego checkpointu jest cache'owany obok (`$WORKDIR/repos/<repo>/gator-index/<job>`),
   żeby nie liczyć hashy całego repo co turę.
3. Diff tury to `git diff --numstat` i `git diff` między checkpointami `n-1` i `n`, z limitem
   rozmiaru (np. 2 MB, pliki binarne tylko jako zmienione).
4. Podsumowanie (pliki, `+`/`-`) idzie w `finish` tury, a pełny diff osobnym uploadem.
5. Refy checkpointów nie są pushowane. Runner usuwa je razem z worktree; diffy żyją na serwerze.
6. `tier=chat` nie robi checkpointów, bo jego checkout jest tylko do odczytu.

**Serwer** (`00050_turn_checkpoints.sql`):

```sql
CREATE TABLE turn_checkpoints (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id      uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    turn_id     uuid REFERENCES agent_turns(id) ON DELETE SET NULL,
    seq         integer NOT NULL,              -- numer tury w jobie
    repo        text NOT NULL,                 -- projekty z wieloma repo
    base_commit text NOT NULL,
    head_commit text NOT NULL,
    files       jsonb NOT NULL,                -- [{path, added, removed, status, binary}]
    diff_key    text,                          -- storage jak załączniki (dysk lub S3)
    diff_bytes  bigint NOT NULL DEFAULT 0,
    truncated   boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (job_id, repo, seq)
);
```

**API:**

```
GET /jobs/{jobId}/checkpoints                    lista tur z podsumowaniem plików
GET /checkpoints/{id}/diff?ignoreWhitespace=     pełny diff tury (unified)
GET /jobs/{jobId}/diff?from=&to=                 diff kilku tur naraz (składany na serwerze)
```

Karta `job` dostaje pod spodem linię „3 pliki, +42 −7” i przycisk „Zmiany”. To ta sama akcja
`diff`, którą karta `artifact` już ma.

**Gotowe, gdy:** po każdej turze `work` jest checkpoint, diff tury nie zawiera zmian z rebase'u, a
gałąź `gator/…` i indeks agenta są nietknięte (test na prawdziwym repo w `executor`).

**Ryzyka:**
- duże repo, gdzie `git add -A` na każdą turę jest drogi (dlatego cache indeksu i pomiar
  `checkpoint_ms` w metrykach);
- pliki z sekretami w workspace: diff idzie na serwer, więc stosuje się do niego ta sama redakcja co
  do logów, a ścieżki z `DenyRead` są wyłączone z checkpointu.

---

## 7.3 Natywny widok diffu i komentarze na liniach

**Po co.** Przegląd pracy agenta ma się dziać w Gatorze, a nie po przejściu na GitHuba. Komentarz
na linii to najprecyzyjniejszy sposób, żeby powiedzieć agentowi, co poprawić.

**Aplikacja (gator-app).**
- Widok diffu przeniesiony z `T3ReviewDiffView.swift` i `ReviewDiffCodeLayout.swift` (MIT, nota w
  nagłówkach). Zostają: sticky nagłówki plików, word-diff i notatki na liniach. Bazę `ExpoView` i
  propsy JSON zastępujemy `UIViewRepresentable` z modelem Swift. Na macOS: port do AppKit
  (`NSViewRepresentable`) albo Catalyst, decyzja po spike'u (1–2 dni).
- Drzewo plików z licznikami, przełącznik „ta tura / cały job”, ignorowanie białych znaków.
- Komentarz na linii: zaznaczenie zakresu linii, pole tekstowe, zapis jako szkic.
- „Wyślij uwagi do @worker” zbiera szkice w jedną wiadomość w wątku taska. Komentarze idą jako
  struktura, nie jako tekst do sparsowania.

**Serwer** (`00051_review_comments.sql`):

```sql
CREATE TABLE review_comments (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    checkpoint_id uuid NOT NULL REFERENCES turn_checkpoints(id) ON DELETE CASCADE,
    author_id     uuid NOT NULL REFERENCES users(id),
    path          text NOT NULL,
    side          text NOT NULL CHECK (side IN ('old', 'new')),
    line_start    integer NOT NULL,
    line_end      integer NOT NULL,
    body          text NOT NULL,
    message_id    uuid REFERENCES messages(id) ON DELETE SET NULL,  -- wiadomość, z którą poszły do agenta
    resolved_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);
```

```
POST   /checkpoints/{id}/comments           szkic komentarza
GET    /jobs/{jobId}/comments               komentarze joba (do nałożenia na diff)
POST   /jobs/{jobId}/comments/send          {agentId?} → wiadomość z komentarzami w wątku taska, wzmianka agenta
PATCH  /review-comments/{id}                treść, resolved
```

**Do agenta.** Pakiet rozmowy z fazy 2 (`conversations/context.go`) dostaje część „Uwagi do
zmian”: plik, zakres linii, fragment kodu z checkpointu i treść uwagi. Komentarz jest pisany przez
członka projektu, więc to nie są dane z zewnątrz. Mimo to fragment kodu idzie w znacznikach, żeby
nie mieszał się z poleceniem.

Wysłanie uwag do agenta `work` przechodzi przez consent jak każda wzmianka (faza 2): bez zgody na
tasku agent odpowiada w `chat` i pojawia się karta „Pozwolić @worker…”.

**Gotowe, gdy:** komentarz na linii w aplikacji trafia do następnej tury @worker z plikiem,
zakresem i fragmentem, a po jego zmianie diff następnej tury pokazuje poprawkę. Snapshoty widoku
diffu w obu motywach.

---

## 7.4 Claude jako żywa sesja

**Po co.** Dziś każda tura i każdy steer to nowy proces `claude -p` z `--resume`. Steer kosztuje
restart (start sesji, ponowne wczytanie MCP), a zgody na narzędzia są niemożliwe, bo proces nie
czeka na odpowiedź. Wzór: `ClaudeAdapterV2.ts:650, 1694, 1830-2231` w t3code, który używa
`@anthropic-ai/claude-agent-sdk` z kolejką wiadomości jako wejściem i control API (`interrupt`,
`setPermissionMode`, `setModel`).

**Runner** (`internal/runner/backend/claude`, nowy tryb `session`):
- Jeden proces na job z wejściem i wyjściem stream-json (`--input-format stream-json
  --output-format stream-json`) zamiast `-p` z promptem w argumencie.
- Kolejna wiadomość użytkownika (steer) to linia `user` na stdin, bez restartu.
- `interrupt` i zmiana trybu uprawnień przez control requests protokołu stdio.
- Prośba o zgodę na narzędzie przychodzi jako control request i czeka na odpowiedź (7.5).
- Wznowienie po padzie runnera: dzisiejsze `--resume <session>`; zgody, które czekały, przepadają
  (`ApprovalsLiveOnly`).
- `executor` przestaje przerywać przy steerze, gdy backend ma `ActiveSteering`. Ścieżka
  `errSteered` zostaje dla backendów bez tej możliwości.

**Do zweryfikowania przed implementacją (spike 2–3 dni).** Dokładne flagi i format control
protocol w wersji Claude Code, którą mają runnery: nazwy requestów `interrupt` i `can_use_tool`
oraz flaga, która kieruje prośby o zgodę na stdio. Trzeba też sprawdzić, czy wywołanie Agent SDK
z Go przez mały helper w Node nie jest prostsze niż własna implementacja protokołu. Wynik spike'u
to nagrania w `testdata` i decyzja zapisana w tym pliku.

**Zachowanie tierów się nie zmienia.**
- `chat`: te same ograniczone narzędzia co dziś (`--tools`, `--allowedTools`, `--disallowedTools`
  z fazy 2). Zgody nie poszerzają `chat`, a prośba o narzędzie zapisu jest odrzucana bez pytania
  człowieka.
- `work`: zgody dotyczą tylko narzędzi spoza polityki projektu (7.5).

**Decyzja po spike'u (zapisana przy implementacji).** Własna implementacja protokołu w Go, bez
helpera w Node: protokół stdio Claude Code to linie JSON w obie strony i kilka control requestów,
a helper dokładałby runnerowi środowisko uruchomieniowe i drugi proces do pilnowania. Flagi wzięte z
`claude --help` 2.1.332 (zapisane w `testdata/claude-2.1.332-help.txt`): `--print --input-format
stream-json --output-format stream-json --verbose --replay-user-messages`, `--permission-prompts
host|none` i tryb `manual` dla pracy ze zgodami. Interrupt to `control_request` z `{"subtype":
"interrupt"}`, prośba o zgodę to `control_request` `can_use_tool` z odpowiedzią `{"behavior":
"allow","updatedInput":…}` albo `{"behavior":"deny","message":…,"interrupt":…}`. Nagrania sesji są
syntetyczne (`testdata/synthetic-session-*.ndjson`, skrypty odgrywane przez `fake-claude.sh`), bo
prawdziwa sesja jest płatna.

**Gotowe, gdy:** steer w trakcie tury nie restartuje procesu (w nagraniu jeden `session_id` i
brak drugiego `init`), stop kończy turę w mniej niż 2 s bez ubijania procesu, a stary tryb `-p`
działa za flagą `GATOR_CLAUDE_MODE=print` jako fallback.

---

## 7.5 Zgody na narzędzia na żywo

**Po co.** Między „agent może wszystko w worktree” (`work`) a „agent tylko czyta” (`chat`) brakuje
środka. Komenda sieciowa, `git push --force`, wywołanie produkcyjnego MCP mają czekać na człowieka,
a nie blokować cały tier. Wzór: `canUseTool` i `RuntimeRequest` w t3code (`ClaudeAdapterV2.ts:4745-4800`,
`orchestrationV2.ts:1415`) oraz zasady z `docs/user/permission-modes.md`.

**Polityka** (`process_config.policy.approvals`, per projekt, a agent może ją zaostrzyć):

```json
{"approvals": {
  "ask": ["Bash(curl:*)", "Bash(git push:*)", "mcp:sentry/*", "WebFetch"],
  "deny": ["Bash(rm -rf /*)"],
  "timeout": "30m",
  "onTimeout": "deny"
}}
```

Reszta narzędzi tieru `work` jest dozwolona jak dziś.

**Serwer** (`00052_runtime_requests.sql`):

```sql
CREATE TABLE runtime_requests (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id      uuid NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    turn_id     uuid REFERENCES agent_turns(id) ON DELETE SET NULL,
    kind        text NOT NULL CHECK (kind IN ('tool_approval', 'question')),
    tool        text,
    input       jsonb NOT NULL DEFAULT '{}',     -- zredagowane jak logi
    status      text NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'allowed', 'allowed_for_session', 'denied', 'expired', 'cancelled')),
    feedback    text,
    decided_by  uuid REFERENCES users(id),
    decided_at  timestamptz,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
```

- Proto: runner wysyła `runtime_request`, a serwer odpowiada `runtime_decision`. Decyzja jest
  idempotentna po `id`.
- Karta `tool_approval` w wątku taska pokazuje agenta, narzędzie, zredagowane wejście i opcje
  Pozwól / Pozwól do końca sesji / Odmów z uzasadnieniem / Zatrzymaj turę.
- `POST /runtime-requests/{id}/decide {decision, feedback?}`, dostępne dla członka projektu z prawem
  pisania w rozmowie (jak `allow` w fazie 2). Osoba z zewnątrz (`external`) nie decyduje.
- `question` (agent pyta człowieka w trakcie tury, np. AskUserQuestion) używa tej samej tabeli,
  karty z polem odpowiedzi i odpowiedzi wracającej jako `user`.

**Zasady, które przejmujemy:**
- nigdy nie udajemy brakującego trybu automatycznym potakiwaniem;
- każdą prośbę da się odrzucić;
- tekst wysłany do wątku, gdy zgoda czeka, to odmowa z tym tekstem jako uzasadnieniem i jednocześnie
  steer;
- prośby subagentów pokazują się w wątku rodzica;
- po timeoucie prośba jest `expired`, a agent dostaje odmowę.

**Gotowe, gdy:** `curl` w turze `work` z domyślną polityką daje kartę, Odmów z uzasadnieniem
zostaje zapisane w nagraniu jako odmowa z tekstem, a karta znika z „Needs you” po decyzji z
innego urządzenia.

---

## 7.6 Steer czy kolejka

**Po co.** Odpowiedź człowieka w trakcie tury jest dziś zawsze steerem (faza 2). Czasem to ma być
następne zadanie po skończeniu bieżącego. Wzór: `inputIntent` w `orchestrationV2.ts:1758` i
`docs/user/composer.md`.

- `messages.input_intent` (`00053_message_intent.sql`): `turn_start | steer | queued | promoted_to_steer`.
- W composerze przy aktywnej turze agenta są dwa przyciski: **Steruj** (domyślnie) i **Do kolejki**
  (⌘⏎ odwraca wybór).
- Kolejka wiadomości jest zapisana na serwerze, widoczna nad composerem, z edycją, usunięciem i
  „Wyślij teraz jako steer”.
- Po zakończeniu tury serwer startuje następny turn z pierwszej wiadomości z kolejki.
- Komendy `/` czekają do końca tury.
- Backend bez `ActiveSteering` (7.1): Steruj działa jak dziś (przerwanie i wznowienie), z
  informacją w podpowiedzi.

---

## 7.7 Załączniki dla agenta

**Po co.** Faza 1 przechowuje załączniki, ale agent ich nie widzi. Zrzut ekranu z błędem czy log
to częsty kontekst. Wzór: `provider/userInputAttachments.ts`, `AttachmentClaims.ts` w t3code.

- **Aplikacja:**
  - upload w composerze (przycisk, przeciągnięcie, wklejenie obrazka) przez istniejące
    `POST /attachments`;
  - wklejka powyżej 32 KiB staje się załącznikiem tekstowym zamiast treścią wiadomości.
- **Serwer:** lease joba odpowiedzi dostaje listę załączników z wiadomości w pakiecie rozmowy, z
  prawem odczytu sprawdzanym jak dla rozmowy (prywatny kanał, DM).
- **Runner:**
  - pobiera pliki tokenem joba do `$WORKDIR/attachments/<job>/`, **poza worktree** (nie trafiają do
    gita ani do checkpointu);
  - podaje backendowi ścieżki: Claude dostaje obrazy jako bloki obrazu w wiadomości `user` (tryb
    sesji z 7.4), resztę jako ścieżki w pakiecie;
  - backend bez `Attachments` dostaje tylko ścieżki;
  - pliki są usuwane razem z worktree.
- **Limity:** rozmiar jak w fazie 1 (25 MB na plik), najwyżej 10 plików na turę, a typy
  wykonywalne tylko jako ścieżka i nigdy jako obraz.

---

## 7.8 Domknięcia po fazie 6 i stan „rozliczanie”

- **Widok taska** filtruje joby po `kind`: domyślnie tylko `phase`, a odpowiedzi i streszczenia są
  pod przełącznikiem „Pokaż odpowiedzi w rozmowie”. „Is running”, „nic do uruchomienia” i pasek faz
  patrzą tylko na joby `phase`.
- **„Pisze…”:** `EventStream` w GatorKit wysyła `{"typing": "<conversationId>"}` najwyżej co 3 s
  podczas pisania. Serwer już to obsługuje (`api/ws_conversations.go`).
- **Tura skończona a rozliczona:** po `finish` job przechodzi w `settling` (checkpoint, upload
  diffu, przebudowa `working_state`, karta), a `done` dopiero po zapisaniu tych rzeczy. Czas i koszt
  tury liczą się do `finish`. Karta pokazuje „kończy…” zamiast przeskakiwać od razu.
  `00054_job_settling.sql` dodaje stan do CHECK-a.
- **Etap startu z prawdziwych sygnałów:** runner wysyła `startup_phase` (`checkout`, `mcp`,
  `initialize`, `ready`) zamiast domyślnego „pracuje”. Karta joba pokazuje, na czym stoi start.
- **Commit message, nazwa gałęzi i tytuł PR generowane tanim modelem** z digestu i diffu tury
  (`policy.roles.summarizer`, jak streszczenia z fazy 2). Gałąź dalej ma prefiks `gator/`.

---

## 7.9 Powiadomienia

- **Push** (APNs, tabela `devices` z 00009) dla:
  - `tool_approval` i `question` adresowanych do mnie (osoba `on_behalf_of` turnu albo
    przypisany do taska);
  - „agent skończył” dla turnów, które sam wywołałem;
  - wzmianki z fazy 1.
- **Akcje w powiadomieniu iOS:** Pozwól / Odmów dla `tool_approval`. Odmów otwiera aplikację, żeby
  dopisać uzasadnienie.
- **Live Activity (ActivityKit)** dla turnu `work`, który wywołałem: etap startu, liczba narzędzi,
  ostatnie narzędzie, czeka na zgodę. Dane z `job.progress` (faza 2).
- **Ustawienia:** reguła jak `NotifyRule` (już jest dla decyzji), rozszerzona o zgody i zakończone
  tury.

---

## Migracje (numeracja orientacyjna)

| Część | Migracja |
|---|---|
| 7.2 | `00050_turn_checkpoints.sql` |
| 7.3 | `00051_review_comments.sql` |
| 7.5 | `00052_runtime_requests.sql` |
| 7.6 | `00053_message_intent.sql` |
| 7.8 | `00054_job_settling.sql` |

`00045_brain_embeddings.sql` wciąż jest zarezerwowana (faza 3), więc obowiązuje uwaga o
`allow-missing` z [README](README.md).

## Podział na PR-y

1. Proto v4: `Capabilities`, raportowanie z runnera, `Job.capabilities` w API, chowanie akcji w aplikacji.
2. Helper `backend/replay` i nagrania ACP dla wspieranych agentów.
3. Spike trybu sesji Claude: nagrania, decyzja (własny protokół czy helper SDK) dopisana tutaj.
4. Checkpointy w runnerze (temp index, `commit-tree`, ukryte refy, cache indeksu) z testem na prawdziwym repo.
5. `turn_checkpoints`, upload diffu, `GET /jobs/{id}/checkpoints`, `GET /checkpoints/{id}/diff`, linia na karcie `job`.
6. gator-app: widok diffu (port z t3code, nota MIT, `THIRD_PARTY_NOTICES.md`), drzewo plików, snapshoty.
7. `review_comments`, komentarze w aplikacji, „Wyślij uwagi do @…”, część „Uwagi do zmian” w pakiecie rozmowy.
8. Backend Claude w trybie sesji: steer bez restartu, natywny interrupt, fallback `print`.
9. `runtime_requests`, karta `tool_approval` i `question`, `POST /runtime-requests/{id}/decide`, polityka `approvals`.
10. `input_intent`, kolejka na serwerze, przyciski Steruj i Do kolejki w composerze.
11. Załączniki: upload w composerze, lease z załącznikami, runner poza worktree, obrazy dla Claude.
12. Domknięcia: filtr jobów w tasku, wysyłanie „pisze…”, `settling`, `startup_phase`, generowane commit message i tytuły.
13. Push i Live Activity dla zgód i zakończonych turnów.

## Gotowe, gdy (cała faza)

- Po turze @worker widzę w aplikacji diff tylko tej tury, komentuję dwie linie, wysyłam uwagi, a
  następna tura je poprawia. Wszystko bez otwierania GitHuba.
- Steer w trakcie tury Claude nie restartuje sesji, a stop działa w mniej niż 2 s.
- `git push --force` agenta czeka na moją zgodę z karty albo z powiadomienia na telefonie, a
  odmowa z uzasadnieniem wraca do agenta.
- Zrzut ekranu wklejony w wątku jest widoczny dla agenta jako obraz.
- Każdy wspierany backend ma nagranie i macierz możliwości, a aplikacja nie pokazuje akcji, których
  backend nie wykona.

## Ryzyka

- **Control protocol Claude Code może się zmieniać.** Pomagają nagrania per wersja CLI, fallback
  `GATOR_CLAUDE_MODE=print` i wersja CLI w `runners.auth_state`.
- **Zmęczenie zgodami.** Za szeroka lista `ask` zamieni pracę agenta w klikanie. Domyślna lista
  jest krótka, „Pozwól do końca sesji” istnieje, a w metrykach liczymy zgody na turę i czas
  oczekiwania.
- **Koszt checkpointów w dużych repo.** Cache indeksu, pomiar `checkpoint_ms` i możliwość
  wyłączenia per projekt (`process_config.checkpoints.enabled`).
- **Port widoku diffu na macOS.** UIKit nie przenosi się na AppKit wprost. Spike decyduje między
  Catalyst a własnym widokiem AppKit. W najgorszym razie macOS dostaje prostszy widok SwiftUI, a iOS
  pełny.
- **Licencja.** Przeniesiony kod Swift musi mieć notę MIT. PR z portem nie przechodzi review bez
  `THIRD_PARTY_NOTICES.md`.

## Odstępstwa od planu (zapisane przy implementacji)

- **Migracje** mają numery z planu, 00050–00054, z kolumnami ponad plan:
  - `turn_checkpoints`: diff całego joba do tej tury (`whole_files`, `whole_key`, `whole_diff`,
    `whole_bytes`, `whole_truncated`), `took_ms` (`checkpoint_ms`) i `diff`/`whole_diff` jako `bytea`
    dla serwera bez magazynu załączników (z magazynem: klucze `diff_key`, `whole_key`);
  - `review_comments`: `snippet` (kod z diffu, zapisany przy tworzeniu) i `updated_at`;
  - `runtime_requests`: `id` nadaje runner (powtórzona prośba to ta sama prośba), `rule`,
    `tool_use_id`, `stopped`, CHECK „pending ⇔ bez `decided_at`”;
  - `messages.input_intent` jest NULL dla wiadomości pisanej bez aktywnej tury;
  - `00054` przebudowuje też indeksy aktywnych jobów o `settling`.
  `store/review_migrations_test.go` robi na osobnej bazie: wejście na 00049 z jobem i rozmową, up,
  dane wszystkich pięciu migracji, CHECK-i, down do 00049 (job `settling` wraca jako `running`) i
  znów up.
- **Proto v4.** Struktura z planu nazywa się `proto.BackendCapabilities` (`Capabilities` to już
  możliwości runnera) i leży w `Capabilities.PerBackend`; heartbeat powtarza ją tylko po zmianie.
  Serwer mówi v4 i v3; runner v2 jest odrzucany przy rejestracji. Runner v3 dostaje joby jak
  dotąd: bez checkpointów, zgód i załączników. `Checkpoints` jest prawdą dla każdego backendu
  (checkpoint robi runner, nie CLI).
- **Nagrania i replay.** Golden files leżą w `testdata/golden/` obok nagrań, a test, który ich nie
  znajduje, nie przechodzi (`GATOR_UPDATE_GOLDEN=1` je pisze). Nagrania ACP i sesji Claude są
  syntetyczne, więc nazywają się `synthetic-*`, a nie `claude-<wersja>-session-*`.
- **Tryb sesji** jest domyślny w runnerze (`GATOR_CLAUDE_MODE=session`); zerowa wartość
  `claude.Backend.Mode` to `print`, więc testy i kod sprzed fazy dalej działają jak wcześniej. CLI,
  który odrzuca flagi sesji (`unknown option` przed `init`), przełącza backend na `print` na stałe
  (`Fallback`), a następny heartbeat mówi o mniejszych możliwościach. Wejście sesji zamyka się po
  `result`, gdy CLI odtworzył wszystkie wysłane wiadomości (`--replay-user-messages`); bez odtwarzania
  — od razu po `result`. Steer, którego sesja nie zdążyła przyjąć, wraca do executora i idzie
  wznowionym runem (`--resume`), więc nic nie ginie. Stop: interrupt, do 2 s na `result`, zamknięcie
  wejścia, grace, kill grupy. Koszt sesji to wartości ostatniego `result` (zakładamy, że są
  narastające) — do sprawdzenia na żywo.
- **Checkpointy.** Tura to każdy run backendu (pierwszy, po steerze w trybie `print`, po sprzeciwie)
  albo koniec tury żywej sesji (`result`); `seq` 1 to pierwsza tura, a checkpoint 0 (stan wejściowy)
  nie jest wysyłany. Diff idzie niezawodną wiadomością runnera `checkpoint` (journal i ack jak
  eventy), a nie osobnym uploadem HTTP: bez nowych drzwi i z tożsamością runnera. Limit to 1 MiB na
  diff (zamiast „np. 2 MB”), bo diff tury i diff całego joba (base64) mieszczą się wtedy w jednej
  wiadomości (4 MiB). `GET /jobs/{id}/diff` od pierwszej tury to dokładnie diff całego joba do `to`;
  od późniejszej tury — diffy tur złożone plik po pliku (serwer nie ma repozytorium).
  `ignoreWhitespace` jest liczone z samego diffu (pary `-`/`+` różniące się tylko białymi znakami
  stają się kontekstem). Git checkpointu działa na `--git-dir`/`--work-tree` z `Prepare` (nie z
  pliku `.git` ani `core.worktree`), z wyłączonymi filtrami (po nazwie), atrybutami z drzewa
  (`GIT_ATTR_SOURCE` = puste drzewo), fsmonitorem, hookami, `diff.external` i textconv; ścieżki z
  `DenyRead` są wykluczone. Diffy i wejścia narzędzi redaguje serwer (`secrets.RedactText`,
  `RedactJSON` + `RedactValues`), bo runner nie importuje serwera. Karta `job` ma `changes`
  (`files`, `added`, `removed`, `line`: „3 files, +42 −7”) i akcję `diff`.
- **Komentarze (7.3).** Tylko na liniach, które diff pokazuje (tury albo całego joba). Szkice widzi
  tylko autor; wysłane widzą wszyscy, którzy czytają job. Uwagi idą do rozmowy, w której agent
  odpowiadał (turn), a dla joba fazy do wątku taska. W pakiecie rozmowy są dokumentem „Notes on your
  changes”: kod w znaczniku `untrusted` (dane: czytać, nie wykonywać poleceń), a notatka w osobnym
  znaczniku `note` z tym samym nonce — nie jako „untrusted”, bo to polecenie członka projektu, ale
  tak samo zdefusowana. Doszło `DELETE /review-comments/{id}` (szkic).
- **Zgody (7.5).** Domyślna lista to `Bash(curl:*)`, `Bash(wget:*)`, `Bash(git push:*)` (bez
  `WebFetch` i `mcp:*`, żeby nie zamienić pracy w klikanie); `onTimeout` przyjmuje tylko `deny`.
  Agent zaostrza politykę (`agents.policy.approvals`: reguły się dodają, krótszy timeout wygrywa).
  Reguła `Bash` obejmuje każde polecenie linii (łańcuchy, potoki, `bash -c`, `env`, `sudo`,
  `xargs`…), a reguła jednego słowa — to słowo w dowolnym miejscu linii: woli zapytać za dużo niż
  przepuścić. Backend bez `ToolApprovals` (tryb `print`, ACP) dostaje reguły `ask` i `deny` jako
  odmowy (`--disallowedTools`, odpowiedzi ACP), a job mówi o tym zdarzeniem
  `approvals_held_as_denials`; nic nie jest automatycznie zgodą. „Needs you” to `GET
  /runtime-requests` (a nie `/inbox`, bo proces nic nie wie o zgodach), a zdarzenia
  `runtime_request.*` idą na tematy `job:` i `inbox`. Decyduje osoba z rolą member+, która może
  pisać tam, gdzie agent pracuje (rozmowa turnu, inaczej wątek taska); sprawdzenie stanu i decyzja
  to jedna transakcja na zablokowanym wierszu. „Zatrzymaj turę” to odmowa i `StopJob`. Pytanie
  (`AskUserQuestion`): odpowiedź to `allow` z treścią w `feedback`; runner odpowiada CLI odmową z
  informacją, a odpowiedź dopisuje jako wiadomość `user`. Prośby wygasają co 15 s (sweeper), a te,
  które czekały, gdy job się skończył, są anulowane (`job.finished`). Prośby subagentów przychodzą z
  tego samego procesu, więc są w wątku rodzica bez osobnej obsługi. **Nie zrobione:** metryki
  zgód na turę i czasu oczekiwania (materiał jest w `job_events`: `runtime_request`,
  `runtime_decision`).
- **Steer czy kolejka (7.6).** Wiadomość z kolejki nie pyta i nie steruje. Po końcu tury
  (`job.finished` joba odpowiedzi) pierwsza z kolejki startuje następną turę i pyta agentów, dla
  których była kolejkowana, obok wzmiankowanych (`message.dequeued`). „Wyślij teraz jako steer” to
  `POST /messages/{id}/steer`, tylko dla autora. Tura zakończona bez joba (odmowa, stop przed
  startem) kolejki nie rusza; następna wiadomość z kolejki czeka na koniec kolejnego joba w rozmowie.
- **Załączniki (7.7)** dotyczą jobów odpowiedzi (pakiet rozmowy): najwyżej 10 plików, najpierw pytania,
  potem najnowsze. Agent pobiera je przez `GET /agent/attachments/{id}` (jobscope `attachment
  read`, tylko reply), który oddaje tylko pliki rozmowy turnu (i wiadomości-korzenia jej wątku), zawsze
  jako `application/octet-stream`. Runner trzyma je w `$WORKDIR/attachments/<job>/` (0700, pliki
  0600). Obraz to PNG, JPEG, GIF lub WebP po bajtach, do 5 MB; program po nazwie albo bajtach
  (skrypt, ELF, Mach-O, PE) jest tylko ścieżką. Obrazy jako bloki obrazu ma tylko tryb sesji.
- **Domknięcia (7.8).** Runner wysyła zdarzenie `settling` zaraz po pętli agenta (przed digestem,
  gitem i pushem); serwer przenosi job z `running`/`stalled` w `settling` i emituje `job.settling`.
  Kolejne zdarzenia nie przywracają `running`; job `settling` bez zdarzeń też staje się `stalled`
  (i wraca wtedy jako `running`). Czas i koszt bez zmian: usage liczy backend. `startup_phase`
  (`checkout`, `mcp`, `initialize`, `ready` przy `init` sesji) idzie zdarzeniem outboxu
  `job.startup` na kartę (`startup`), do pierwszego narzędzia. `GET /tasks/{id}/jobs?kind=` filtruje
  po rodzaju. „Pisze…” należy do aplikacji. **Nie zrobione:** commit message, nazwa gałęzi i tytuł PR
  z tanim modelem — to nowy rodzaj joba po stronie serwera, a nazwa gałęzi jest ustalana przed pracą;
  gałąź dalej ma `gator/<task>-<job>`.
- **Powiadomienia (7.9).** Push `TOOL_APPROVAL`/`QUESTION` idzie do osoby `on_behalf_of` turnu, a bez
  niej do właściciela taska (nie do wszystkich członków); `TURN_FINISHED` do osoby, która zapytała,
  gdy odpowiedź jest gotowa. Odbiorca musi móc czytać rozmowę agenta. Reguły rejestruje się w
  `Notifier.On`; APNs niesie `category` i dane (`request_id`). **Nie zrobione po stronie serwera:**
  Live Activity (tokeny i aktualizacje ActivityKit) i ustawienia osoby dla nowych reguł — aplikacja
  filtruje po kategorii.
- **CLI:** `gator job checkpoints`, `gator job diff`, `gator approvals`, `gator decide`.
- **Po przeglądzie** (szczegóły w commicie): wyłączenia filtrów checkpointu idą przez
  `GIT_CONFIG_KEY_n`/`VALUE_n` (nazwa sterownika z `=` nie rozcina już `-c`), a konfiguracja
  globalna nie jest czytana; reguły `mcp:` są tłumaczone na nazwy Claude Code w `--disallowedTools`;
  runner wysyła czekające prośby jeszcze raz po każdym połączeniu (decyzja podjęta, gdy go nie było,
  dochodzi), a prośby nie trafiają do journala (zgody są tylko na żywo); job wzięty ponownie zaczyna
  checkpointy od nowa (stare są usuwane, klucze diffów mają numer próby); sesja zamyka wejście po
  `UnreadWait` także wtedy, gdy odtworzenia wiadomości nie da się dopasować; `settling` przeżywa
  ponowienie zdarzenia i zastój; „Needs you” filtruje projekty osoby w SQL; ścieżka z diffu w
  wiadomości z uwagami jest oczyszczona. Prośby są anulowane też przy `job.requeued`.
- **Po przeglądzie PR #28** (szczegóły w commicie):
  - checkpoint nie daje gitowi czytać plików worktree: runner listuje nazwy (`ls-files`), czyta pliki
    przez `os.Root` worktree (żaden link, podmieniony choćby w trakcie, nie wyprowadza poza niego),
    sam zapisuje bloby i oddaje gitowi gotowe wpisy (`update-index --index-info`); żaden filtr ani
    atrybut repozytorium nie jest więc wykonywany, kiedykolwiek go skonfigurowano;
  - reguły `Bash` zdejmują opcje gita przed podkomendą (`-C`, `-c`, `--git-dir`…), `eval`,
    `sudo -u X`, `env -u X VAR=…`, `command`, `exec`, `xargs -n N`; to dalej próg, nie piaskownica:
    skrypt zapisany do pliku i uruchomiony przechodzi;
  - backend, który przestał pytać w trakcie joba (sesja → `print`), odmawia także reguł `ask`
    (`Access.Ask` zawsze dochodzi do backendu, a `--disallowedTools` niesie `ask` i `deny`);
  - steer turnu `work` tylko od członka projektu, który może pisać w rozmowie; inaczej wiadomość idzie
    dalej jak zwykła, przez strażników; odmowa czekającej prośby wiadomością tylko, gdy pisze osoba
    turnu, w DM-ie, w wątku agenta albo ze wzmianką agenta;
  - runner v3 nie dostaje jobów `work` projektu, który o cokolwiek pyta (polityka domyślna pyta), i
    takie joby czekają jak bez runnera (`no_runner`), aż przyjdzie runner v4;
  - sesja: czytnik i nadzór startują przed pierwszym zapisem, zamknięcie wejścia nie czeka na zapis
    (zamknięcie rury kończy zapis zablokowany), steer jest „applied” dopiero po odtworzeniu przez CLI
    (albo po końcu tury, gdy CLI nie odtwarza), a steer, którego CLI nie przeczytało przed śmiercią,
    wraca jako nieprzeczytany; po przejściu na `print` tura dalej ma checkpoint;
    `control_cancel_request` przestaje pytać człowieka, a runner mówi serwerowi
    (`runtime_cancel`), że już nie czeka; wejście narzędzia powyżej 64 KiB jest odrzucane, nie
    pokazywane w skrócie; błąd czytania wyjścia CLI kończy run od razu;
  - wiadomość z kolejki pamięta agentów, dla których była kolejkowana (`messages.queued_for`), i
    pyta właśnie ich; „Wyślij teraz” wiadomości, która nikogo nie pyta, to 409;
  - checkpointy mają numer próby (`attempt`): job wzięty ponownie zaczyna tury od nowa, a komentarze
    ludzi zostają przy checkpointach wcześniejszej próby; listy plików są ucinane z flagą
    (`filesTruncated`), żeby wiadomość zmieściła się w 4 MiB; karta liczy zmiany wszystkich repozytoriów;
  - `GET /runners` i `GET /runners/{id}` pokazują tylko joby, które wołający może czytać;
  - migracje 00050, 00053 i 00054 poprawione w miejscu (PR nie był scalony): `attempt`,
    `files_truncated`, `queued_for`, indeks zdarzeń cyklu życia joba.
  - **Przygotowanie pod PR #27:** środowisko procesu agenta buduje jedna funkcja
    (`backend.AgentEnv`, ta sama nazwa i podpis co w #27), a wszystko, co job wysyła na serwer w
    trakcie (zdarzenia, koniec tury, checkpointy z diffami i ścieżkami, prośby z wejściem narzędzia),
    przechodzi przez jeden haczyk `client.JobIO.Masked(mask)` — tu zastąpi `maskIO` z #27.
- **Zawężone:** kolejka wiadomości rusza tylko na końcu joba odpowiedzi; ponownie dostarczone
  `job.finished` zanim następna tura wystartuje może wziąć z kolejki drugą wiadomość. Pliki diffów
  wcześniejszej próby joba zostają w magazynie (wiersze są usuwane).

## Do zrobienia w gator-app

Wszystko poniżej ma już API w `docs/openapi.yaml` (regeneracja `GatorAPI`): endpointy, zdarzenia
WebSocket fazy (`job.checkpoint`, `job.settling`, `job.startup`, `runtime_request.*`,
`message.dequeued`, z treścią) i kategorie pushy z danymi i linkami są opisane w `info.description`.

1. **Możliwości.** `Job.capabilities` (i `RunnerInfo.capabilities.perBackend`, `GET
   /runners/{id}`): chować Steer, natywny Stop, karty zgód i podpowiedź „Steruj przerywa turę”, gdy
   job ich nie obsłuży.
2. **Widok diffu** przeniesiony z t3code (`T3ReviewDiffView.swift`, `ReviewDiffCodeLayout.swift`):
   nota MIT w nagłówkach plików, `THIRD_PARTY_NOTICES.md`, nazwy bez „T3”, `UIViewRepresentable`
   zamiast `ExpoView` i propsów JSON; macOS po spike'u (AppKit albo Catalyst). Sticky nagłówki plików,
   word-diff, notatki na liniach. Źródła: `GET /jobs/{id}/checkpoints` (tury, pliki, `+`/`-`), `GET
   /checkpoints/{id}/diff?whole=&ignoreWhitespace=`, `GET /jobs/{id}/diff?from=&to=` (diff unified,
   `text/x-diff`); `truncated` mówi, że diff ucięto na 1 MiB.
3. **Drzewo plików** z licznikami, przełącznik „ta tura / cały job” (`whole=true` albo `/jobs/{id}/diff`),
   ignorowanie białych znaków.
4. **Komentarze na liniach:** zaznaczenie zakresu po stronie `old`/`new`, `POST
   /checkpoints/{id}/comments` (szkic), `GET /jobs/{id}/comments` do nałożenia na diff, `PATCH`
   (treść szkicu, `resolved`), `DELETE` szkicu, „Wyślij uwagi do @worker” = `POST
   /jobs/{id}/comments/send {agentId?}`.
5. **Karta `job`:** linia `card.changes.line` i przycisk „Zmiany” (akcja `diff`), stan `settling`
   („kończy…”), `card.startup` (na czym stoi start).
6. **Karty `tool_approval` i `question`** (klucz `approval:<id>`): Pozwól, Pozwól do końca sesji,
   Odmów z uzasadnieniem, Zatrzymaj turę; pytanie — pole odpowiedzi. `POST
   /runtime-requests/{id}/decide`. „Needs you”: `GET /runtime-requests`, aktualizowane zdarzeniami
   `runtime_request.*` (temat `inbox`).
7. **Composer:** Steruj / Do kolejki przy aktywnej turze (⌘⏎ odwraca), `NewMessage.intent`; kolejka
   nad composerem (`GET /conversations/{id}/queue`, `Message.inputIntent`) z edycją, usunięciem i
   „Wyślij teraz jako steer” (`POST /messages/{id}/steer`).
8. **Załączniki w composerze:** przycisk, przeciągnięcie, wklejony obrazek przez `POST /attachments`;
   wklejka powyżej 32 KiB jako załącznik tekstowy.
9. **Widok taska:** joby `kind=phase` domyślnie (`GET /tasks/{id}/jobs?kind=phase`), odpowiedzi i
   streszczenia pod przełącznikiem; „is running”, „nic do uruchomienia” i pasek faz tylko z jobów fazy.
10. **„Pisze…”:** `{"typing":"<conversationId>"}` przez WebSocket najwyżej co 3 s.
11. **Powiadomienia:** kategorie `TOOL_APPROVAL` (Pozwól, Odmów — Odmów otwiera aplikację),
    `QUESTION`, `TURN_FINISHED` z `request_id`/`turn_id` w danych; Live Activity dla własnego turnu
    `work` z `job.progress` i `startup_phase`; ustawienia jak `NotifyRule`.
12. **Snapshoty** widoku diffu w obu motywach.

## Do sprawdzenia na żywo

- Tryb sesji Claude Code na prawdziwym CLI: flagi, `--replay-user-messages` (czy odtwarza wiadomości
  tak, jak je wysłano), steer w trakcie tury (jeden `session_id`, jeden `init`), interrupt i czas
  stopu, `--permission-prompts host` z `--permission-mode manual` (czy pyta o wszystko poza odczytem),
  kształt `can_use_tool` i odpowiedzi, `AskUserQuestion` (czy odmowa z odpowiedzią jako wiadomość
  działa), bloki obrazu, narastający koszt w `result`, prośby subagentów.
- `--disallowedTools` z regułami `Bash(curl:*)` w `bypassPermissions` (tryb `print`).
- Koszt checkpointów na dużym repozytorium (`checkpoint_ms` w zdarzeniu `checkpoint`).
- APNs z kategorią i danymi na urządzeniu.

# Faza 1: Rozmowy

**Cel:** ludzie z projektu rozmawiają w kanałach, wątkach i DM-ach, a praca (taski, decyzje, joby)
pojawia się w rozmowie jako karty. Agenci w tej fazie jeszcze nie odpowiadają (to faza 2), ale model
danych od początku przewiduje ich jako członków.

**Poza zakresem:** odpowiedzi agentów, Brain, mosty zewnętrzne, załączniki większe niż obrazki i pliki
do 25 MB.

## Model pojęć

| Pojęcie | Opis |
|---|---|
| **Conversation** | `channel` (nazwany, w projekcie), `dm` (2–8 uczestników, poza projektem lub w nim), `thread` (odpowiedzi pod jedną wiadomością), `task` (wątek przypisany do taska, jeden na task) |
| **Member** | `user` albo `agent` (polimorficznie: `member_kind`, `member_id`) |
| **Message** | `text` albo `card`; autor to user, agent albo system; edycja, usunięcie (soft), reakcje |
| **Card** | wiadomość strukturalna z typem i payloadem: `task`, `decision`, `job`, `artifact`, `pr`, `incident`, `release`, `brain_proposal` (faza 3) |

Domyślne kanały tworzone z projektem: `#general`. Opcjonalnie `#incidents` (gdy projekt ma plugin z
capability `monitoring`) i `#<component>` dla komponentu z katalogu (włączane ręcznie).

**Wątek ↔ task.** Utworzenie taska (API, plugin, automation) tworzy conversation `kind='task'` i
wrzuca kartę `task` do kanału projektu (domyślnie `#general`, a w `process_config.conversations.task_channel`
można wskazać inny). Odpowiedzi pod kartą *są* wątkiem taska. Działa to też w drugą stronę: akcja
„Zrób z tego task" na dowolnej wiadomości tworzy task, linkuje istniejący wątek jako jego conversation
i kopiuje do opisu taska streszczenie oraz link do wiadomości.

## Dane (`00035_conversations.sql`)

Pełna migracja: `internal/server/store/migrations/00035_conversations.sql`. Skrót:

```sql
CREATE TABLE conversations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   uuid REFERENCES projects(id) ON DELETE CASCADE,   -- NULL dla DM poza projektem (i jego wątków)
    kind         text NOT NULL CHECK (kind IN ('channel', 'dm', 'thread', 'task')),
    name         text,                                              -- tylko channel; unikalny w projekcie
    topic        text NOT NULL DEFAULT '',
    visibility   text NOT NULL DEFAULT 'project' CHECK (visibility IN ('project', 'private')),
    parent_message_id uuid,                                         -- thread/task: wiadomość-korzeń
    task_id      uuid UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,-- kind='task'
    component_key text,
    dm_key       text,                                              -- tylko dm: posortowane id uczestników
    archived_at  timestamptz,
    created_by_kind text NOT NULL CHECK (created_by_kind IN ('user','agent','system','plugin','external')),
    created_by   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_message_at timestamptz,
    last_seq     bigint NOT NULL DEFAULT 0,                         -- seq najnowszej wiadomości
    CHECK ((kind = 'channel') = (name IS NOT NULL)),
    CHECK ((kind = 'dm') = (dm_key IS NOT NULL)),
    CHECK ((kind = 'task') = (task_id IS NOT NULL)),
    CHECK (kind <> 'thread' OR parent_message_id IS NOT NULL),
    CHECK (kind IN ('dm', 'thread') OR project_id IS NOT NULL)
);
-- Kanał zawsze ma projekt, a inne rodzaje nie mają nazwy: indeks częściowy zamiast UNIQUE (project_id, name).
CREATE UNIQUE INDEX conversations_channel_name_idx ON conversations (project_id, name) WHERE kind = 'channel';
-- Jeden DM na zestaw ludzi w danym zakresie; NULL project_id to jeden zakres („poza projektami”).
CREATE UNIQUE INDEX conversations_dm_idx ON conversations (project_id, dm_key) NULLS NOT DISTINCT WHERE kind = 'dm';
CREATE UNIQUE INDEX conversations_parent_idx ON conversations (parent_message_id) WHERE parent_message_id IS NOT NULL;

CREATE TABLE conversation_members (
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    member_kind  text NOT NULL CHECK (member_kind IN ('user', 'agent')),
    member_id    uuid NOT NULL,
    role         text NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member')),
    notify       text NOT NULL DEFAULT 'mentions' CHECK (notify IN ('all', 'mentions', 'none')),
    last_read_seq bigint NOT NULL DEFAULT 0,
    joined_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_id, member_kind, member_id)
);

CREATE TABLE messages (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    seq             bigint NOT NULL,                 -- monotoniczny w conversation; paginacja i unread
    author_kind     text NOT NULL CHECK (author_kind IN ('user', 'agent', 'system', 'plugin', 'external')),
    author_id       uuid,
    kind            text NOT NULL DEFAULT 'text' CHECK (kind IN ('text', 'card')),
    body            text NOT NULL DEFAULT '',        -- Markdown (podzbiór)
    card_type       text,
    card            jsonb,
    card_key        text,                            -- o czym jest karta ("job:<id>"); karta jest aktualizowana, nie dublowana
    reply_count     integer NOT NULL DEFAULT 0,
    status          text NOT NULL DEFAULT 'final' CHECK (status IN ('streaming', 'final', 'failed')),
    origin          jsonb NOT NULL DEFAULT '{}',     -- np. {"source":"slack","ts":"…"} (faza 5)
    client_id       text,                            -- idempotencja wysyłki z klienta
    edited_at       timestamptz,
    deleted_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'card') = (card_type IS NOT NULL)),
    CHECK (kind = 'card' OR (card IS NULL AND card_key IS NULL)),
    UNIQUE (conversation_id, seq),
    UNIQUE (conversation_id, client_id),
    UNIQUE (conversation_id, card_key)
);
ALTER TABLE conversations ADD CONSTRAINT conversations_parent_message_fk
    FOREIGN KEY (parent_message_id) REFERENCES messages(id) ON DELETE CASCADE;

CREATE TABLE message_mentions (
    message_id  uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    member_kind text NOT NULL CHECK (member_kind IN ('user', 'agent')), member_id uuid NOT NULL,
    PRIMARY KEY (message_id, member_kind, member_id)
);

CREATE TABLE message_reactions (
    message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    member_kind text NOT NULL CHECK (member_kind IN ('user', 'agent')), member_id uuid NOT NULL,
    emoji text NOT NULL CHECK (length(emoji) BETWEEN 1 AND 64),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, member_kind, member_id, emoji)
);

CREATE TABLE message_attachments (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    message_id uuid REFERENCES messages(id) ON DELETE CASCADE,  -- NULL do wysłania wiadomości, która go niesie
    uploaded_by uuid REFERENCES users(id) ON DELETE SET NULL,
    name text NOT NULL, content_type text NOT NULL, size_bytes bigint NOT NULL,
    storage_key text NOT NULL UNIQUE,           -- S3-compatible albo dysk lokalny
    created_at timestamptz NOT NULL DEFAULT now()
);
```

`seq` nadawany jest w transakcji przez `UPDATE conversations SET last_seq = last_seq + 1 RETURNING last_seq`,
co blokuje wiersz rozmowy do końca transakcji i daje kolejność bez luk na conversation.

`00038_message_search.sql`: `tsvector` generowany z `body` (konfiguracja `simple`, bo zespoły piszą
w PL i EN) plus indeks GIN, a do tego `pg_trgm` na nazwy kanałów.

## API (`docs/openapi.yaml`)

```
GET    /projects/{projectId}/conversations?kind=channel          lista kanałów (z unread, last_message_at)
POST   /projects/{projectId}/conversations                       nowy kanał {name, topic, visibility}
GET    /conversations?kind=                                       rozmowy, w których jestem (też DM-y poza projektem)
POST   /conversations/dm                                          {members:[…], projectId?} → istniejący (200) lub nowy (201) DM
GET    /conversations/{id}                                        szczegóły + członkowie + moje uprawnienia
PATCH  /conversations/{id}                                        name, topic, archive
PUT    /conversations/{id}/members/{kind}/{memberId}              dołącz / zmień notify
DELETE /conversations/{id}/members/{kind}/{memberId}
GET    /conversations/{id}/messages?beforeSeq=&afterSeq=&limit=   paginacja po seq
POST   /conversations/{id}/messages                               {body, clientId, mentions[], attachments[]}
GET    /messages/{id}                                             jedna wiadomość (np. po zdarzeniu z WS)
PATCH  /messages/{id}                                             edycja (autor, 24 h) 
DELETE /messages/{id}                                             soft delete (autor albo admin projektu)
PUT    /messages/{id}/reactions/{emoji}  /  DELETE …
GET    /messages/{id}/thread                                      conversation wątku (tworzy przy 1. odpowiedzi)
POST   /messages/{id}/promote                                     „zrób task" {kind, title?, projectId?} → {task, conversationId}
POST   /conversations/{id}/read                                   {seq}
GET    /tasks/{taskId}/conversation
GET    /search/messages?q=&projectId=&limit=                      FTS, tylko w dostępnych conversation
POST   /attachments                                               upload (multipart: conversationId, file) → id
GET    /attachments/{id}                                          plik dla czytających rozmowę
```

**WebSocket:** nowe topiki `conversation:<id>` (wiadomości, edycje, reakcje, typing) i `me`
(unread, mentions, DM-y, czyli wszystko, co dotyczy mnie). Uprawnienia sprawdza się przy subskrypcji,
tak jak dziś dla `project:<id>`.

**Zdarzenia (outbox `events`):** `message.posted`, `message.edited`, `message.deleted`,
`reaction.changed`, `conversation.created`, `conversation.member_changed`. Typing jest ulotny i
nie trafia do outboxu, tylko bezpośrednio do huba.

## Karty: skąd się biorą

Nowy konsument outboxu `internal/server/conversations` (z własnym kursorem `conversations` w `event_cursors`,
tak jak `product`) zamienia zdarzenia domenowe na karty w wątku taska. Każdy rodzaj karty to osobne
źródło (`CardSource`: `cards_task.go`, `cards_decision.go`, `cards_job.go`, `cards_artifact.go`),
a `Projector` zapisuje karty kluczem (`card_key`), więc nowy rodzaj karty to nowe źródło, bez zmian w reszcie:

| Zdarzenie | Karta | Akcje w karcie (te same endpointy co dziś) |
|---|---|---|
| task utworzony | `task` w kanale projektu | otwórz, przypisz |
| bramka czeka na człowieka (pozycja inboxu) | `decision` | Approve → `/tasks/{id}/approve`, Rollback, Handoff, Allow agents |
| job queued/running/done/failed | `job` (jedna karta aktualizowana, a nie nowa na każdy event) | Stop, Steer, Logi |
| nowa wersja artefaktu | `artifact` | podgląd, diff wersji |
| plugin: PR otwarty/zmerge'owany, CI | `pr` | link, status checków |
| incydent / release | `incident` / `release` w `#incidents` lub `#general` | |

Karta odwołuje się do obiektu (`card.ref = {kind, id}`), a klient dociąga aktualny stan, więc stary
stan nie zostaje „zamrożony" w treści wiadomości. Aktualizacja karty to zdarzenie `message.edited` z
nowym `card`.

**Inbox zostaje** jako widok „Needs you", czyli agregat kart `decision` adresowanych do mnie. Źródło
prawdy się nie zmienia (`/inbox`).

## Pakiety serwera

- `internal/server/conversations`: serwis (tworzenie, członkostwo, uprawnienia), `cards.go`,
  `promote.go` i `context.go` (budowa „pakietu rozmowy" dla agenta, używany w fazie 2).
- Uprawnienia: członek projektu widzi kanały `project`; `private` widzą tylko członkowie; `viewer`
  czyta i reaguje, ale nie pisze; DM poza projektem widzą tylko jego uczestnicy. Wspólna funkcja
  `CanRead(conversation, actor)` jest używana przez API, WS i wyszukiwanie.
- Powiadomienia: `internal/server/notify` dostaje regułę „mention / DM / odpowiedź w moim wątku".
  Tabela `devices` (00009) już jest do pushy.
- Audit: POST/PATCH/DELETE wpadają do `audit_log` automatycznie (middleware). Treść wiadomości nie
  trafia do audytu, tylko id.

## Klient (minimum fazy 1, szczegóły w fazie 6)

Lista kanałów w sidebarze, widok rozmowy, composer z @mention ludzi, wątek w prawym panelu,
karty `task` i `decision` z działającymi przyciskami, unread oraz push na mention.

## Podział na PR-y

1. Migracja 00035, zapytania sqlc, serwis conversations z testami uprawnień.
2. Endpointy wiadomości i paginacji, WS `conversation:<id>`, zdarzenia.
3. Kanał `#general` tworzony z projektem plus backfill dla istniejących projektów (migracja 00036).
4. Wątek taska: tworzenie przy tasku, backfill dla otwartych tasków (migracja 00037), `GET /tasks/{id}/conversation`.
5. Konsument kart (`task`, `decision`, `job`, `artifact`).
6. Promote (wiadomość → task).
7. Reakcje, edycja, usuwanie, załączniki.
8. FTS (00038) i `/search/messages`.
9. Powiadomienia (mentions, DM).
10. Regeneracja `GatorAPI` w gator-app i ekrany z fazy 6a.

## Gotowe, gdy

- Dwie osoby rozmawiają w `#general` na żywo, mają unread i push na mention.
- Każdy nowy task ma wątek, a decyzja z bramki jest kartą, którą da się zatwierdzić z rozmowy.
- „Zrób z tego task" działa i task ma link do rozmowy.
- Testy: uprawnienia (viewer, private, DM poza projektem), kolejność `seq` przy równoległych POST-ach,
  idempotencja `client_id`, karty aktualizowane, a nie duplikowane.

## Ryzyka

- **Szum kart.** Task z wieloma jobami zaśmieca wątek. Dlatego jedna karta `job` na job, aktualizowana,
  a kolejne fazy zwijane do jednej linii.
- **Kanał `#general` jako śmietnik kart tasków.** Rozwiązaniem jest konfigurowalny `task_channel`
  oraz filtr „tylko rozmowy" w kliencie.

## Odstępstwa od planu (zapisane przy implementacji)

- **Nazwy parametrów** w API są camelCase jak w reszcie `docs/openapi.yaml` (`beforeSeq`, `afterSeq`,
  `clientId`, `projectId`), a nie snake_case z pierwszej wersji tego dokumentu.
- **Wzmianki** przychodzą od klienta jawnie (`mentions: [{kind, id}]`), a nie z parsowania `@imię` w
  treści: ludzie nie mają unikalnych uchwytów. Osoba, która nie może czytać rozmowy, jest pomijana.
- **Zdarzenia nie niosą treści wiadomości**, tylko id, `seq`, autora i `audience` (kogo dotyczą).
  Klient dociąga wiadomości po `afterSeq`, przez te same reguły dostępu. Treść nie zostaje więc w
  outboxie po usunięciu wiadomości, a pluginy czytające zdarzenia jej nie widzą.
- **Temat `me`** to `user:<id>` w hubie; hub kieruje tam zdarzenia rozmów, których `audience` zawiera
  tę osobę (członkowie rozmowy i wspomniani). Nikt nie subskrybuje `user:<id>` wprost.
- **Typing** wysyła klient przez WebSocket (`{"typing":"<conversationId>"}`), a serwer publikuje
  `conversation.typing` prosto do huba (najwyżej raz na 2 s na rozmowę i połączenie).
- **Dodatkowe zdarzenia:** `conversation.updated` (temat, nazwa, archiwum), `conversation.read`
  (tylko do czytającego, dla jego innych urządzeń), `message.replied` (licznik odpowiedzi pod
  wiadomością-korzeniem, w rozmowie korzenia).
- **Administrator workspace'u** moderuje kanały projektów jak admin projektu, ale prywatny kanał i
  DM czyta tylko wtedy, gdy jest ich członkiem. Rozmowa, której nie wolno czytać, odpowiada 404.
- **`GET /messages/{id}/thread`** otwiera wątek przy pierwszym pytaniu (idempotentnie), z autorem
  wiadomości jako członkiem; wątki się nie zagnieżdżają (400 dla odpowiedzi).
- **Migracje:** backfille `#general` (00036) i wątków otwartych tasków (00037) są osobnymi migracjami,
  a FTS ma numer 00038; numery kolejnych faz przesunęły się o 2 (tabela w `README.md`).
- **`#general` przy projekcie** tworzy `POST /projects` (twórca od razu w nim jest), a osoby dołączające
  do projektu (`project.member_set`, `project.invitation_accepted`) dodaje do niego konsument outboxu
  pakietu (kursor `conversations`). `#general` nie da się przemianować ani zarchiwizować (409).
- **Wątek taska** otwiera konsument outboxu na `task.created` (kartę `task` w kanale zadań, a pod nią
  conversation `kind='task'`), a `GET /tasks/{id}/conversation` robi to samo, jeśli jeszcze nie zdążył;
  jeden wątek na task gwarantuje `UNIQUE (task_id)`. `process_config.conversations.task_channel`
  wskazuje inny kanał niż `#general` (nieistniejący albo zarchiwizowany = `#general`). Backfill 00037
  zakłada wątki otwartym taskom bez karty w kanale (bez wiadomości-korzenia), żeby nie zalać `#general`.
- **Karty** mają klucz w rozmowie (`messages.card_key`): `task:<id>`, `job:<id>`, `artifact:<faza>:<typ>`,
  `decision:<faza>:<wejście w fazę>`. Zapis karty z tą samą treścią nic nie zmienia (powtórzone
  zdarzenie nie robi nic), inna treść to `message.edited`. Karta `decision` pokazuje regułę krótkiego
  inboxu dla jednego taska (`process.Service.DecisionFor`); gdy task przestaje czekać, karta dostaje
  `state: resolved`, a powrót do fazy po rollbacku to nowa karta. Karty `incident`/`release`/`pr` i kanał
  `#incidents` zostają na później (plan PR-ów fazy 1 wymienia w konsumencie tylko `task`, `decision`,
  `job`, `artifact`); `working_state` runnera nie ma karty.
- **Promote** tworzy task przez `process.Service.Create` (te same reguły consentu i `origin`), a
  powiązanie wątku robi w tej samej transakcji (nowe pole `process.CreateParams.InTx`). Wątkiem taska
  zostaje wątek pod wiadomością albo wątek, w którym wiadomość jest odpowiedzią; wiadomość bez
  odpowiedzi dostaje wątek razem z taskiem. Karta `task` trafia wtedy do środka tego wątku (a nie do
  kanału, bo wątek wisi pod wiadomością człowieka). Opis taska cytuje wiadomość i do 5 pierwszych
  odpowiedzi (bez LLM-owego streszczenia) i linkuje `gator://conversations/<id>?seq=<n>`. Wiadomość w
  DM-ie poza projektem wymaga `projectId`. Rodzaje: `feature`, `bug`, `chore`, `incident`.
- **Załączniki** idą przez serwer (multipart, do 25 MB), bez presigned URL-i: dostęp sprawdza ta sama
  `Access`, a klucz do bucketu nie opuszcza serwera. Magazyn: `GATOR_ATTACHMENTS_S3_*` (jak backupy)
  albo `GATOR_ATTACHMENTS_DIR`; bez nich upload odpowiada 503. Plik należy do nikogo, dopóki uploader
  nie wyśle wiadomości z jego id (`message_attachments.message_id` jest NULL do tej chwili). Obrazki
  pokazują się inline, reszta jako pobranie, zawsze z `nosniff` i `sandbox`.
- **Usunięcie wiadomości** zostawia jej miejsce (`seq`, wątek) i czyści treść w bazie; jej pliki nie są
  już wydawane (sprzątanie magazynu zostaje na później). Edycja nie zmienia wzmianek. Karty nie są
  edytowane ręcznie, a usuwa je tylko moderator.
- **FTS (00038)** to indeks wyrażeniowy GIN na `to_tsvector('simple', body)` (bez usuniętych), a nie
  kolumna `tsvector`: to samo wyszukiwanie bez wektora w każdym odczycie wiadomości. SQL zawęża
  kandydatów (otwarte rozmowy czytelnych projektów i te, w których jestem), a każdy wynik i tak
  przechodzi przez `Access`. Zapytanie w składni `websearch_to_tsquery`. `pg_trgm` służy filtrowi
  `?q=` na liście kanałów projektu (`ILIKE` po indeksie trigramowym).

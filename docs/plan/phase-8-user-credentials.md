# Faza 8: własne logowania do modeli

Stan na 2026-11-25. Do tej pory każdy job działał na logowaniu runnera: na tym, co operator wpisał
w jego środowisko (`ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, `GATOR_CLOUD_RUNNER_ENV`) albo co
właściciel Maca miał w keychainie. Po tej fazie osoba po zalogowaniu do aplikacji podłącza własne
konto Claude albo Codex, a joby, za które płaci, idą na jej limitach albo kluczach. Serwer, runner,
CLI i `docs/openapi.yaml` są w tej fazie; ekran w aplikacji (Settings → Model logins) robi faza UI
na Macu według kontraktu. Numer migracji i nazwa fazy idą po fazie 7
([przegląd zmian i żywe sesje](phase-7-review-and-sessions.md)), która zajęła 00050–00054.

## Decyzje (ustalone)

- **Rodzaje logowania**: subskrypcja Claude (token z `claude setup-token`, `CLAUDE_CODE_OAUTH_TOKEN`),
  klucz API Anthropic (`ANTHROPIC_API_KEY`), subskrypcja ChatGPT dla Codexa (`auth.json` z
  `codex login`, z odświeżanymi tokenami), klucz API OpenAI dla Codexa.
- **Kto płaci**: osoba, która poprosiła o job; za fazy procesu twórca taska; za automatyzację osoba,
  która ją włączyła; za streszczenia i kurację Brainu twórca taska albo rozmowy, albo ten, kto je
  wywołał. Bez pasującego logowania job czeka z powodem dla człowieka i nigdy nie bierze cudzego.
- Płatnik to jeden resolver (strategia), łatwy do zmiany.

## Jak to działa

- **Migracja `00055_user_credentials.sql`**: `user_credentials` (rodzaj, backend, sekret
  zaszyfrowany keyringiem serwera, podpowiedź z 4 ostatnich znaków, status, ostatni błąd, kiedy
  sprawdzone i użyte), w `jobs` płatnik, użyte logowanie i powód czekania, w `usage_records` kto
  zapłacił, w `workspace_settings` przełącznik `runner_logins`.
- **`internal/server/credentials`**: rodzaj logowania to `Kind` zarejestrowany w `Registry`
  (`DefaultKinds`); opcjonalnie `Rotating` (CLI przepisuje login, jeden job naraz) i `Checker` (jedno
  wywołanie do dostawcy). Kolejność rejestracji to kolejność preferencji w backendzie: subskrypcja
  przed kluczem API. `Service` trzyma logowania (lista, zapis, usunięcie, sprawdzenie) i implementuje
  `runners.Credentials`.
- **`runners.DefaultPayers`**: łańcuch reguł `PayerRule` — płatnik podany przez wołającego (osoba,
  której tura zleciła streszczenie; twórca rozmowy przy kuracji), osoba, która poprosiła, osoba, w
  której imieniu działa łańcuch tur, włączający automatyzację, twórca taska (z historii przejść),
  osoba, która pozwoliła agentom na tasku. Płatnik jest ustalany przy kolejkowaniu.
- **Lease**: `LeaseCandidates` (SKIP LOCKED) i dla każdego kandydata `Credentials.Choose` w jednej
  transakcji pod `pg_advisory_xact_lock`; login ChatGPT trzymany przez aktywny job jest pominięty, a
  unikalny indeks częściowy na `jobs(credential_id)` jest bezpiecznikiem. Job czekający ma
  `waiting_reason` i zdarzenie `job.waiting` (raz na zmianę powodu); zapis logowania czyści powody
  jego jobów i oferuje kolejkę od nowa.
- **Runner**: `proto.Job.Credential` (zmienne, katalog z plikami, wartości do maskowania). Runner
  mówi `capabilities.payer_logins`; inny dostaje tylko joby na własnym logowaniu. Agent dostaje login
  i żadnej zmiennej logowania runnera (`agentenv.WithoutLogins`); Codex dostaje świeży `CODEX_HOME`
  (0700, `auth.json` 0600), usuwany po jobie. Zmieniony `auth.json` wraca w `Finish.CredentialRefresh`
  i zastępuje zapisany (poza receiptem); odrzucone logowanie wraca w `Finish.CredentialRefused`.
- **Maskowanie**: runner maskuje wartości logowania w zdarzeniach, podsumowaniu, digestach i
  receipcie; serwer maskuje je drugi raz przed zapisem i publikacją (`secrets.Mask`).
- **API**: `GET /me/credentials`, `PUT|DELETE /me/credentials/{kind}`, `POST /me/credentials/{kind}/check`,
  `GET /me/model-usage`, `GET /users/{id}/credentials` (admin, bez wartości), `GET /workspace/model-usage`
  (admin), `GET|PUT /workspace/model-credentials`; `Job.payerId`, `credentialKind`, `waitingReason`;
  zdarzenia `credential.saved|removed|refused` tylko dla właściciela. CLI: `gator credentials …`.

## Odstępstwa od planu

- **Bez nowej wersji protokołu.** Faza 7 rezerwuje proto v4, więc logowanie płatnika jest polem
  addytywnym, a zgodność trzyma lista `capabilities.payer_logins` (backendy, które runner uruchamia
  na loginie płatnika). Runner, który jej nie ma, i backend spoza niej (Claude z `apiKeyHelper` albo
  kluczem w `env` zarządzanych ustawień, które przebijają token płatnika) nigdy nie dostaną loginu
  płatnika ani joba, który by go wymagał.
- **Login nie jest szyfrowany osobno w drodze.** Idzie w leasie przez uwierzytelniony WebSocket
  runnera, jak token joba; poufność w drodze daje TLS. Runner i tak musi mieć go jawnie.
- **Opcja "pożycz logowania runnerów" jest ustawieniem workspace'u, nie runnera.** Nowy workspace ma
  ją wyłączoną; migracja włącza ją w workspace, który już uruchamiał joby, żeby aktualizacja niczego
  nie zatrzymała. Płatnik z własnym logowaniem (nawet odrzuconym albo zajętym) nigdy nie dostaje
  logowania runnera.
- **Sprawdzenie ChatGPT nie istnieje**: jedyne realne wywołanie odświeżyłoby token i zużyło refresh
  token. Status tego logowania mówi to, co powiedział ostatni job.
- **`jobs.payer_id` bez klucza obcego**, jak `jobs.created_by`: rekord joba przeżywa ludzi.
- **Wygasły lease zwalnia login ChatGPT**, choć stary job mógł jeszcze działać; jeśli obaj
  odświeżą, login skończy jako odrzucony i właściciel podłączy go ponownie. Refresh od runnera,
  którego job został już przeleasowany, przepada (`ownJob`).
- **Wykrywanie odrzuconego logowania** to dopasowanie słów (`invalid api key`, wygasły token OAuth,
  `refresh_token_reused`, `authentication required`, 401), bo Claude Code i Codex nie dają stabilnego
  kodu — ale tylko w błędzie samego CLI (wynik `is_error` i stderr Claude Code, błąd RPC adaptera
  ACP), nigdy w tym, co napisał agent. Dla rodzaju, który da się sprawdzić, serwer przed oznaczeniem
  pyta dostawcę; login, który dostawca przyjmuje, zostaje. Job wtedy kończy się porażką z powodem, a
  nie wraca do kolejki.
- **Sprawdzenie subskrypcji Claude jest niepewne**: nie wiadomo, czy `GET /v1/models` przyjmuje
  token OAuth (do sprawdzenia na żywo, lista w README). 401/403 zostawia login jako `unchecked` z notą.
- **Jeden płatnik na runner naraz.** Joby jednego runnera działają jako ten sam użytkownik i czytają
  sobie środowisko (`/proc/<pid>/environ`) i pliki, a żaden runner nie izoluje dziś jobów od siebie:
  job na loginie Ann idzie tylko obok jobów na loginie Ann, nigdy obok Boba ani joba na loginie
  runnera; pozostałe sloty czekają. Login payera trafia tylko na maszynę payera albo runner wspólny
  (bez właściciela), kto by nie zakolejkował joba. Katalog loginu (z własnym `CLAUDE_CONFIG_DIR`, więc
  transkrypty się nie mieszają) leży pod `GATOR_RUNNER_LOGIN_DIR`, a runner sprząta pozostałości po
  awarii przy starcie. Zostaje ryzyko sekwencyjne: to, co job zostawił poza katalogiem loginu, i
  dziennik runnera na Linuksie, czyta następny job.
- **Turn z zewnątrz nie ma płatnika.** Slack, GitHub, A2A albo ktokolwiek bez `OnBehalfOf`: login
  runnera tylko, gdy workspace go pożycza, inaczej czeka. Nikt z zewnątrz nie wydaje loginu członka.
- **Model czyta własne środowisko.** Maskowanie (runner, serwer, wszystko, co agent wysyła swoim
  tokenem: gator-cli, MCP, proxy MCP, to, co czyta Judge) trzyma login z dala od tego, co Gator
  przechowuje i pokazuje, nie od agenta, który chce go wynieść. **Na później**: lokalne proxy po
  stronie runnera, które dokłada uwierzytelnienie do wywołań modelu, tak że agent w ogóle nie widzi
  tokenu.
- **Blokada leasów jest globalna**, nie tylko dla loginów rotujących: który login dostanie job, wiadomo
  dopiero po zablokowaniu jego wiersza, a blokada musi poprzedzać wiersze. Zapis, usunięcie i
  odrzucenie loginu oraz zmiana reguły workspace'u biorą ją pierwsze, więc kolejność jest jedna
  (blokada, joby, login) i nie ma zakleszczeń.
- **Szyfrogram loginu jest związany z (`user_id`, `kind`)** (AAD). Tabela powstaje w 00055, więc nie
  ma starych wierszy do przeszyfrowania.
- **Refresh zastępuje tylko wersję z leasu**: login ma wersję, job zapisuje ją przy leasie, a nowszy
  login, który właściciel zapisał w trakcie, zostaje.
- **Nowa zależność**: `golang.org/x/term` (już w grafie modułów) dla ukrytego wpisywania w
  `gator credentials set`.
- **Zawężone**: powiadomienie o odrzuconym logowaniu to zdarzenie WebSocket `credential.refused` dla
  właściciela (i status w `GET /me/credentials`), bez pusha. Logowanie, które runner trzyma na dysku
  użytkownika (`~/.claude/.credentials.json`, keychain), agent wciąż może przeczytać; Claude Code
  woli to ze środowiska, a Codex czyta tylko podany `CODEX_HOME`.

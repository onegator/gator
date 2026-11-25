# Nowy kierunek: rozmowy, agenci, Brain

Stan na 2026-11-24. Plan zmiany kierunku Gatora z „narzędzia procesu" na **przestrzeń roboczą
zespołu, w której ludzie i agenci rozmawiają ze sobą, a każda rozmowa jest przypięta do pracy**.
Misja się nie zmienia: jedna aplikacja dla zespołu, który robi oprogramowanie od discovery po
monitoring produkcji.

Inspiracja: [nightly-labs/openbot](https://github.com/nightly-labs/openbot). Bierzemy z niego wyłącznie
pomysły, bo licencja PolyForm Noncommercial wyklucza kopiowanie kodu.

## Zasady, które obowiązują we wszystkich fazach

1. **Rozmowa jest przypięta do pracy.** Każdy wątek może stać się taskiem, a każdy task ma swój
   wątek. Nie budujemy ogólnego czatu, który konkuruje ze Slackiem funkcjami. Konkurujemy tym, że
   decyzja, plan, PR i incydent dzieją się w rozmowie.
2. **Proces zostaje kręgosłupem.** Fazy, bramki, zatwierdzenia, consent, budżety i metryki
   (`internal/server/process`, `jobs`, `usage_records`) nie są przepisywane. Rozmowa to nowy
   interfejs do nich.
3. **Agent jest członkiem, a nie funkcją.** Ma tożsamość (`@reviewer`), profil, rolę, budżet i
   uprawnienia. Wszystko, co zrobi, jest przypisane do niego i do człowieka, który go poprosił.
4. **Wiedza przechodzi przez człowieka.** Agent proponuje, człowiek zatwierdza. Do promptu trafia
   tylko to, co zatwierdzone, tak jak dziś w `product_context`. Każdy wpis Brainu ma pochodzenie.
5. **Sekrety nie opuszczają serwera.** Integracje, w tym zdalne serwery MCP, idą przez gator-server
   (proxy). Runner dostaje token joba, nigdy klucz do zewnętrznego systemu.
6. **Słowa z zewnątrz to dane, nie polecenia.** Wiadomość ze Slacka, komentarz w PR czy alert trafia
   do agenta w znacznikach untrusted, tak jak dziś obiekcje pluginów.
7. **Mierzymy, nie deklarujemy.** Każda nowa ścieżka do agenta zapisuje koszt, kontekst
   (`context_bytes`) i cytowania, żeby dało się sprawdzić, czy Brain pomaga.

## Fazy

| # | Faza | Cel | Zależy od | Szacunek |
|---|---|---|---|---|
| 1 | [Rozmowy](phase-1-conversations.md) | kanały, wątki, DM-y, wiadomości, karty, wątek ↔ task | — | 3 tyg. |
| 2 | [Agenci jako członkowie](phase-2-agents.md) | tożsamość agenta, @mention → odpowiedź w wątku, delegacja | 1 | 3 tyg. |
| 3 | [Brain](phase-3-brain.md) | jeden magazyn wiedzy z zakresami, kontekst użytkownika, serwer MCP | 1, 2 | 4 tyg. |
| 4 | [Marketplace](phase-4-marketplace.md) | paczki agentów i skilli, katalog integracji, proxy MCP | 2, (3) | 3 tyg. |
| 5 | [Otwarcie na zewnątrz](phase-5-external.md) | backend ACP w runnerze, A2A, most do Slacka, komentarze GitHub | 2, 4 | 4 tyg. |
| 6 | [UI](phase-6-ui.md) | trzy panele, rozmowa z kartami, Brain, Marketplace, opcjonalny klient web | 1 → równolegle | 4 tyg. + web |

Szacunki zakładają 1–2 osoby z agentami. Fazy 1 i 6a idą razem, bo rozmowa bez ekranu nie istnieje.

```
1 Rozmowy ──► 2 Agenci ──► 3 Brain ──► 4 Marketplace ──► 5 Zewnętrzne
   │              │                                  ▲
   └── 6a UI ─────┴── 6b UI (Brain, Marketplace) ────┘    6c web (decyzja)
```

## Pierwszy pionowy wycinek (koniec fazy 2)

W kanale `#<projekt>` piszę „@reviewer zobacz PR #12". Agent odpowiada w wątku i strumieniuje
odpowiedź na żywo, a koszt widać pod wiadomością. Jednym kliknięciem „Zrób z tego task" wątek staje
się taskiem `bug` albo `feature`, z tą rozmową jako pierwszym kontekstem. Decyzja „zatwierdź plan"
przychodzi jako karta w wątku taska i działa tym samym endpointem co dziś `/tasks/{id}/approve`.

## Decyzje przyjęte jako założenia (do potwierdzenia)

| Pytanie | Założenie w planie | Co zmienia inna odpowiedź |
|---|---|---|
| Klient natywny czy web? | SwiftUI zostaje głównym klientem; web jako faza 6c, start po fazie 2 | web od razu = +4–6 tyg. przed fazą 3 |
| Zastąpić Slacka czy most? | nasze kanały są pierwszorzędne, Slack przez most w fazie 5 | bez mostu faza 5 maleje o ~1,5 tyg. |
| Kontekst użytkownika w Brainie | domyślnie prywatny, udostępnianie per wpis | domyślnie zespołowy = prostsze reguły, większe ryzyko |
| Marketplace | kurowany rejestr Gatora; społeczność później, z podpisami | otwarty od razu = review, sandbox, zgłaszanie nadużyć w fazie 4 |

## Migracje (numeracja orientacyjna)

| Faza | Migracja |
|---|---|
| 1 | `00035_conversations.sql`, `00036_general_channels.sql`, `00037_task_conversations.sql`, `00038_message_search.sql` |
| 2 | `00039_agents.sql`, `00040_agent_turns.sql`, `00041_agents_keep.sql` |
| 3 | `00042_brain.sql`, `00043_brain_backfill.sql`, `00044_brain_curation.sql`, `00045_brain_embeddings.sql` (opcjonalna, pgvector; numer zarezerwowany, jeszcze nie napisana) |
| 4 | `00046_marketplace.sql`, `00047_mcp_servers.sql` |
| 5 | `00048_external_agents.sql`, `00049_messaging_bridges.sql` |

Faza 1 dostała dwie migracje więcej niż w pierwszej wersji planu: backfill `#general` i backfill
wątków otwartych tasków to osobne kroki (osobne PR-y), więc numery kolejnych faz przesunęły się o 2.
Faza 2 dostała jedną więcej (`00041_agents_keep.sql`: agentów się odkłada, a nie usuwa, i turn jest
zajmowany przed startem), więc fazy 3–5 przesunęły się o kolejną. Faza 3 dostała jedną więcej
(`00044_brain_curation.sql`: dokąd curator przeczytał każdą rozmowę), więc embeddingi mają 00045, a
fazy 4–5 przesunęły się o jeszcze jedną.

Faza 4 zmieściła się w dwóch zaplanowanych numerach: `00046_marketplace.sql` (rejestry, cache
paczek, instalacje, skille) i `00047_mcp_servers.sql` (serwery MCP, `mcp_calls` i `target` w
`plugin_connect_states` dla OAuth serwerów MCP). 00045 wciąż czeka na embeddingi; kto ją napisze,
uruchamia `goose` z `allow-missing` na bazach, które mają już 00046 (goose odrzuca brakującą
migrację starszą niż ostatnia wykonana).

Serwery MCP rejestru Gatora (`gator-marketplace/integrations`) mają adresy i narzędzia „na dziś”, do
sprawdzenia z żywymi usługami przed publikacją: Sentry (`https://mcp.sentry.dev/mcp`), Atlassian
(`https://mcp.atlassian.com/v2/mcp`; v1 jest wycofywane), Notion (`https://mcp.notion.com/mcp`), Figma
(`https://mcp.figma.com/mcp`), Slack (`https://mcp.slack.com/mcp`) — czy każdy publikuje metadata
RFC 9728/8414 na swoim hoście i rejestrację RFC 7591 (inaczej potrzebna aplikacja operatora z
`GATOR_<PROVIDER>_OAUTH_HOSTS`) i jak nazywają się ich narzędzia; Datadog
(`https://mcp.datadoghq.com/api/unstable/mcp-server/mcp`) jest eksperymentalny (klucze API i aplikacji w
dwóch nagłówkach, adres zależny od regionu), a Grafana jest samodzielnie hostowana (adres ustawia admin).

Każda ma krok down, a migracje uruchamia się tylko na żądanie (`gator-server migrate`), jak dotąd.

## Co celowo NIE wchodzi

- Wideo, huddle, kalendarz, edytor dokumentów: zostają w narzędziach zespołu i dochodzą integracjami.
- Własny model językowy czy embeddingi trenowane u nas.
- Uruchamianie agentów na desktopie członka zespołu jak w OpenBocie. U nas agent działa na runnerze
  (VPS lub Mac), a desktop jest tylko klientem.

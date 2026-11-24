# Faza 6: UI

**Cel:** aplikacja otwiera się na rozmowie, a nie na dashboardzie. To, co wymaga mojej decyzji, jest
na górze. Praca agentów jest widoczna w wątkach, a kontekst (task, artefakty, Brain, log joba) jest
zawsze pod ręką w prawym panelu.

Repo: `gator-app` (SwiftUI, `GatorKit/Sources/GatorUI`, snapshoty w `GatorUITests`). Fazę dzielimy:
- **6a** (razem z fazami 1–2): powłoka trzech paneli, rozmowa, karty, agenci;
- **6b** (z fazami 3–4): Brain, Marketplace, profil „Ja";
- **6c** (decyzja): klient web.

## Układ

```
┌───────────────┬───────────────────────────────────────┬────────────────────────────┐
│ ⌘K Szukaj     │ # general · Gator API          ⓘ  ⋯  │ Wątek / Kontekst           │
│               ├───────────────────────────────────────┤                            │
│ ● Needs you 3 │ Ania  10:12                            │ ▸ Task: Logowanie Apple    │
│               │ możemy dodać Sign in with Apple?       │   faza: planning · @planner│
│ PROJEKTY      │   └ 4 odpowiedzi · @planner pracuje…   │   [Approve plan] [Rollback]│
│ ▾ Gator API   │                                        │ ▸ Artefakty: brief v2, plan│
│   # general   │ ┌ TASK  Logowanie Apple ─────────────┐ │ ▸ Brain: 3 wpisy użyte     │
│   # incidents │ │ feature · discovery ✓ · planning ⏳ │ │   • decyzja: OIDC only…    │
│   # auth      │ └────────────────────────────────────┘ │ ▸ Job (live)  $0.42 · 3m   │
│ ▸ Landing     │                                        │   Read internal/auth/…     │
│               │ @reviewer  10:20  ·  $0.18             │                            │
│ WIADOMOŚCI    │ Przejrzałem PR #12. Dwa problemy: …    │                            │
│   Ania        │                                        │                            │
│   🤖 @planner │ ┌────────────────────────────────────┐ │                            │
│               │ │ Napisz… @ agent / komenda   ⏎      │ │                            │
│ AGENCI        │ └────────────────────────────────────┘ │                            │
│ 🤖 @reviewer ●│                                        │                            │
│ 🤖 @worker    │                                        │                            │
│               │                                        │                            │
│ Brain · Market│                                        │                            │
│ ⚙ Ustawienia  │                                        │                            │
└───────────────┴───────────────────────────────────────┴────────────────────────────┘
```

**Lewy panel (`Sidebar`):**
1. **Needs you**: dawny „Decisions" (inbox). Licznik na ikonie w docku.
2. **Projekty** z kanałami (unread pogrubiony, wzmianka jako badge).
3. **Wiadomości**: DM-y z ludźmi i agentami.
4. **Agenci** projektu z kropką statusu (pracuje, bezczynny, wstrzymany).
5. Na dole: Brain, Marketplace, Ustawienia.

Dzisiejsze miejsca: **Board** i **Dashboard** zostają widokami projektu (przełącznik w nagłówku
kanału: Rozmowa | Board | Jakość | Release'y). **Runners, Integrations, People, Policy, Automations**
przechodzą do Ustawień (projekt / workspace), bo to konfiguracja, nie codzienna praca.

**Środek:** lista wiadomości (leniwa, paginacja po `seq` w górę) z kartami i composerem.

**Prawy panel (inspector):** wątek otwartej wiadomości **albo** kontekst taska wątku: fazy, karta
decyzji, artefakty, użyte wpisy Brainu, live job (dzisiejszy log z `TaskDetailView`). Można go
zwinąć, a na iOS staje się arkuszem.

## Komponenty do zbudowania (GatorUI)

| Komponent | Opis | Snapshoty |
|---|---|---|
| `ConversationView` | lista + composer, skok do nieprzeczytanych, „nowe wiadomości" | pusta, długa, z unread, offline |
| `MessageRow` | autor (człowiek/agent/zewnętrzny z oznaczeniem), Markdown, reakcje, edytowano, koszt pod agentem | każdy `author_kind`, `streaming`, `failed` |
| `CardView` + typy | `TaskCard`, `DecisionCard`, `JobCard`, `ArtifactCard`, `PRCard`, `IncidentCard`, `BrainProposalCard`, `ConsentCard` | każdy typ × stan |
| `Composer` | @autocomplete (ludzie + agenci z opisem), `/` komendy, wklejanie obrazków, ⌘⏎ | z podpowiedziami |
| `ThreadInspector` | wątek w prawym panelu | |
| `TaskInspector` | wyciągnięty z `TaskDetailView` | |
| `AgentProfileView` | opis, rola, tier, model i backend, budżet, statystyki, skille, narzędzia, Pauza | builtin, marketplace, remote |
| `NeedsYouView` | karty `decision` ze wszystkich projektów, grupowane | |
| `CommandPalette` (⌘K) | skok do kanału, taska, agenta, wpisu Brainu | |

**Komendy w composerze:**

```
/task [kind] tytuł           nowy task z wątkiem tutaj
/ask @agent pytanie          jak wzmianka, ale wymusza tryb chat
/brain szukaj…               wyniki inline; /brain propose tytuł
/promote                     zrób task z bieżącego wątku
/invite @osoba               do kanału
```

## 6b: Brain i Marketplace

**Brain:**
- **Szukaj**: pole, filtry (zakres, rodzaj, status), wyniki z pochodzeniem.
- **Propozycje**: kolejka z Approve / Edit / Reject, grupowana dziennie, z podglądem sprzeczności.
- **Wpis**: treść, wersje (oś czasu supersede), linki, „gdzie użyto" (joby, które go dostały lub zacytowały).
- **Ja**: moja ekspertyza, preferencje, fokus (wyliczony); przełącznik widoczności per wpis;
  eksport i „usuń mój kontekst".

**Marketplace:**
- zakładki Agenci / Skille / Integracje / Procesy, filtr fazy SDLC (Discovery → Monitoring) jako
  oś na górze (to *nasza* opowieść: od pomysłu po produkcję);
- karta paczki: ikona, opis, wydawca, wersja, wymagania (czego brakuje w projekcie), przykładowy prompt;
- instalacja: arkusz z uprawnieniami do zaakceptowania (checkboxy = `accepted_permissions`), a po
  instalacji od razu „Przywitaj się z @qa" (otwiera DM z agentem);
- zainstalowane: wersja, chip Update, diff uprawnień.

## Onboarding (nowy)

1. Logowanie (bez zmian).
2. „Stwórz projekt" → repo → wybór startowego zestawu agentów (domyślni są zaznaczeni) → zaproś
   ludzi (link).
3. Wylądowanie w `#general` z wiadomością powitalną od `@planner`: „Opisz, co budujecie. Zrobię z
   tego wizję i pierwsze wpisy do Brainu". To jest pierwszy turn i pierwsza propozycja Brainu, czyli
   wartość w 2 minuty.

## GatorKit

- `ConversationStore`: cache wiadomości per conversation (okno + paginacja), merge zdarzeń WS
  (`message.posted` / `edited`, reakcje), optymistyczna wysyłka z `client_id`, ponowienie po reconnect.
- Subskrypcje WS: `me` zawsze; `conversation:<id>` dla otwartej rozmowy i otwartego wątku;
  `project:<id>` dla widocznego projektu (jak dziś Board).
- `GatorAPI` regenerowany z `openapi.yaml` (pin `GATOR_REF`), bez ręcznych edycji.
- Powiadomienia: macOS UserNotifications + APNs na iOS z tabeli `devices`; akcja „Odpowiedz" w
  powiadomieniu.

## 6c: Klient web (decyzja)

Argument za: zapraszanie ludzi spoza Maca (PM, design, klient), linki do wątków, które otwierają się
wszędzie, i to, że produkt „jak Slack" bez weba jest trudny do sprzedania. Argument przeciw: drugi
klient do utrzymania.

Jeśli tak, to rekomendacja:
- osobne repo `gator-web`, TypeScript, klient generowany z tego samego `openapi.yaml`;
- serwowany przez gator-server pod `/app` (statyczne pliki w release), sesja cookie z istniejącego
  OIDC (`gator_session` już jest);
- zakres pierwszego wydania: rozmowy, wątki, karty, Needs you, Brain (czytanie i zatwierdzanie).
  Konfiguracja zostaje w aplikacji natywnej.
- start po fazie 2, szacunek 4–6 tyg.

## Podział na PR-y (gator-app)

1. Powłoka: nowy `SidebarItem` (needsYou, channel(id), dm(id), agent(id), brain, marketplace, settings);
   Runners/Integrations/People/Policy/Automations przeniesione do `SettingsView`.
2. `ConversationStore` + WS + testy merge'owania zdarzeń.
3. `ConversationView`, `MessageRow`, `Composer` (bez komend), snapshoty.
4. Karty `task`, `decision`, `job` z akcjami.
5. Inspector: wątek i task (refaktor `TaskDetailView`).
6. @autocomplete z agentami, karta consentu, strumieniowanie odpowiedzi agenta.
7. `AgentProfileView`, DM z agentem.
8. Komendy `/` i ⌘K.
9. Needs you, powiadomienia.
10. (6b) Brain: Szukaj, Propozycje, Wpis, Ja.
11. (6b) Marketplace i instalacja.
12. Onboarding.

## Gotowe, gdy

- Nowa osoba zaproszona linkiem w ciągu 5 minut: dołącza, pisze w `#general`, dostaje odpowiedź od
  agenta i zatwierdza swoją pierwszą decyzję z karty, nie otwierając Boardu.
- Wszystkie nowe komponenty mają snapshoty w jasnym i ciemnym motywie.
- Lista 10 000 wiadomości przewija się płynnie (leniwa lista, pomiar w Instruments).

## Ryzyka

- **Utrata „mapy procesu".** Dzisiejsza siła (Board, fazy) może zginąć w rozmowach. Dlatego karta
  taska zawsze pokazuje pasek faz, a Board jest jednym kliknięciem z nagłówka kanału.
- **Przeładowanie kartami.** Filtr w nagłówku kanału: Wszystko | Tylko rozmowy | Tylko praca.

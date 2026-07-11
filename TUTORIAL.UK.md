# Туторіал pgc - від нуля до типізованих запитів

Це повний покроковий шлях: ти піднімеш PostgreSQL у Docker, напишеш
міграцію й кілька анотованих SQL-запитів, згенеруєш типізований Go-пакет
через pgc і використаєш його зі справжньої програми. Кожна команда
розрахована на копіювання і запуск як є; нічого, крім передумов, не
припускається. Закладай близько двадцяти хвилин.

Англійська версія: **[TUTORIAL.md](TUTORIAL.md)**.
Довідник: **[DOC.UK.md](DOC.UK.md)**.

## Зміст

- [0. Передумови](#0-передумови)
- [1. Створення проєкту](#1-створення-проєкту)
- [2. PostgreSQL у Docker](#2-postgresql-у-docker)
- [3. Встановити pgc](#3-встановити-pgc)
- [4. Вказати pgc базу](#4-вказати-pgc-базу)
- [5. Написати й застосувати міграцію](#5-написати-й-застосувати-міграцію)
- [6. Спитати сервер про запит](#6-спитати-сервер-про-запит)
- [7. Файл запитів](#7-файл-запитів)
- [8. Мова анотацій](#8-мова-анотацій)
- [9. Конфігурація генерації: pgc.json](#9-конфігурація-генерації-pgcjson)
- [10. Генерація](#10-генерація)
- [11. Використання згенерованого коду](#11-використання-згенерованого-коду)
- [12. Цикл змін](#12-цикл-змін)
- [13. Чесність у CI](#13-чесність-у-ci)
- [14. Розв'язання проблем](#14-розвязання-проблем)

## 0. Передумови

Два інструменти, кожен перевіряється одним рядком:

```sh
go version        # потрібен go1.24 або новіший
docker compose version
```

Якщо немає `go` - встанови з <https://go.dev/dl/>. Якщо немає
`docker compose` - встанови Docker Desktop або пакети docker-ce +
compose-plugin своєї системи. PostgreSQL на машині не потрібен - він
житиме в контейнері.

## 1. Створення проєкту

```sh
mkdir notes && cd notes
go mod init example.com/notes
```

`go mod init` оголошує Go-модуль. Шлях `example.com/notes` - лише ім'я;
якщо плануєш публікувати проєкт, використай справжній шлях репозиторію
(`github.com/you/notes`).

## 2. PostgreSQL у Docker

Створи `docker-compose.yaml` у корені проєкту:

```yaml
services:
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: secret
      POSTGRES_DB: app
    ports:
      - "127.0.0.1:5433:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U app -d app"]
      interval: 1s
      retries: 30
```

Рядок за рядком:

- **image** - офіційний образ PostgreSQL 17, варіант Alpine (малий).
- **environment** - налаштування першого запуску: користувач `app` з
  паролем `secret` і база `app` створюються автоматично.
- **ports** - мапінг `"127.0.0.1:5433:5432"` читається як
  *адреса-хоста:порт-хоста:порт-контейнера*: сервер слухає 5432 всередині
  контейнера, а з твоєї машини доступний як `127.0.0.1:5433`. Порт 5433
  обрано, щоб ніколи не зіткнутися з PostgreSQL, який, можливо, вже
  крутиться локально на 5432; прив'язка до `127.0.0.1` ховає його від
  мережі.
- **healthcheck** - дає `docker compose` знати, коли сервер справді
  готовий, а не просто запущений.

```sh
docker compose up -d
docker compose ps
```

Повторюй `docker compose ps`, доки сервіс `db` не покаже `(healthy)` - на
першому запуску найдовше тягнеться образ.

## 3. Встановити pgc

```sh
go install github.com/goloop/pgc@latest
pgc version
```

`go install` кладе бінарник у `$(go env GOPATH)/bin` - зазвичай
`~/go/bin`. Якщо `pgc version` каже "command not found", додай ту теку в
PATH:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

Дві альтернативи:

- **Готові бінарники** для Linux, macOS і Windows причеплені до
  [релізів](https://github.com/goloop/pgc/releases), з контрольними
  сумами - завантаж, розпакуй, поклади `pgc` на PATH.
- **Зі сирців**: `make build` у клоні репозиторію дає `./pgc`.

Якщо реліз затегували хвилини тому, Go module proxy міг його ще не
побачити; `GOPROXY=direct go install github.com/goloop/pgc@latest` тягне
прямо з репозиторію.

## 4. Вказати pgc базу

pgc читає URL з'єднання з оточення - ніколи з конфіг-файлу, щоб облікові
дані не потрапляли в репозиторій:

```sh
export PGC_DATABASE_URL="postgres://app:secret@127.0.0.1:5433/app?sslmode=disable"
```

Анатомія URL, точно відповідна compose-файлу:

```
postgres://  app  :  secret  @  127.0.0.1 : 5433  /  app  ?sslmode=disable
схема        юзер    пароль     хост        порт     база    опції
```

`sslmode=disable` правильний для локального контейнера - там нема TLS.
Для віддалених серверів бери `require` або `verify-full`.

## 5. Написати й застосувати міграцію

Міграція - це звичайний SQL-файл, що рухає схему на крок уперед;
`pgc migrate` застосовує кожен файл рівно один раз, у порядку імен. Створи
`migrations/001_init.sql`:

```sh
mkdir migrations
```

```sql
CREATE TYPE note_status AS ENUM ('draft', 'published');

CREATE TABLE notes (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title      text NOT NULL,
    body       text NOT NULL DEFAULT '',
    status     note_status NOT NULL DEFAULT 'draft',
    tags       text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
```

Що ці типи колонок дадуть пізніше: `GENERATED ALWAYS AS IDENTITY` -
сучасний автоінкремент, `note_status` - справжній enum (стане Go-типом з
константами), `text[]` - масив (стане `[]string`), а `timestamptz` -
`time.Time`.

Застосуй:

```sh
pgc migrate
```

```
applied 001_init.sql
```

Кожен файл виконується у власній транзакції разом зі своїм обліковим
рядком у таблиці `pgc_migrations`, тож невдала міграція не лишає по собі
нічого - виправ файл і запусти ще раз. Другий `pgc migrate` скаже
`nothing to apply`: файли застосовуються рівно один раз, а редагування вже
застосованого файлу дає попередження, а не повторний запуск (пиши
наступну міграцію - схема рухається лише вперед). `pgc migrate status`
показує, що застосовано, а що чекає. Нумеровані імена (`001_...`,
`002_...`) тримають порядок стабільним, поки проєкт росте.

Перевір тим `psql`, що всередині контейнера:

```sh
docker compose exec db psql -U app -d app -c '\dt'
```

Маєш побачити `notes` і `pgc_migrations`.

## 6. Спитати сервер про запит

Перш ніж щось генерувати, подивись на головну ідею наживо. pgc не парсить
SQL - він просить запущений сервер *описати* стейтмент (підготувати без
виконання), і сервер повідомляє кожен тип:

```sh
pgc describe "SELECT id, title, tags FROM notes WHERE id = $1"
```

```
server 17.10

Parameters:
  $1   int8

Columns:
  id                   int8         not null  notes.id
  title                text         not null  notes.title
  tags                 _text        not null  notes.tags
```

Сервер це сказав - отже, згенерований код цьому відповідатиме. Битий
запит відкидається тут-таки, з точною позицією помилки - до твого Go-коду
ніколи не доїде нічого хибного.

## 7. Файл запитів

Створи `queries/notes.sql`. Кожен запит - звичайний SQL з маленьким
анотаційним заголовком; текст коментаря під `-- name:` стає godoc-ом
згенерованого методу:

```sql
-- name: CreateNote :one
-- Inserts a note and returns it.
INSERT INTO notes (title, body, tags)
VALUES ($1, $2, $3)
RETURNING id, title, body, status, tags, created_at;

-- name: GetNote :one
-- Returns one note by primary key.
SELECT id, title, body, status, tags, created_at
FROM notes
WHERE id = $1;

-- name: ListNotes :many
-- Returns notes, newest first.
SELECT id, title, body, status, tags, created_at
FROM notes
ORDER BY created_at DESC, id DESC
LIMIT $1 OFFSET $2;

-- name: SearchByTag :many
-- Returns every note carrying the tag.
-- param: $1 tag
SELECT id, title, body, status, tags, created_at
FROM notes
WHERE $1 = ANY(tags)
ORDER BY id;

-- name: PublishNote :execrows
-- Marks a note as published and reports how many rows changed.
UPDATE notes
SET status = 'published'
WHERE id = $1;

-- name: CountNotes :one
-- override: total int64 notnull
SELECT count(*) AS total
FROM notes;

-- name: IterNotes :iter
-- Streams every note, oldest first.
SELECT id, title, body, status, tags, created_at
FROM notes
ORDER BY id;

-- name: DeleteNote :exec
DELETE FROM notes
WHERE id = $1;
```

Дві анотації, крім `-- name:`, уже з'явилися вище й заслуговують на слово
зараз (повна мова - в наступному розділі):

- `-- param: $1 tag` називає перший аргумент `tag`. pgc виводить імена
  аргументів із SQL (`id = $1` називає його `id`, `LIMIT $1` - `limit`) і
  відкочується до `argN`, коли SQL не дає підказки - анотація і є явним
  запасним виходом.
- `-- override: total int64 notnull` поправляє одну колонку результату:
  `count(*)` - вираз, каталог не може обіцяти, що він ніколи не NULL, тож
  pgc зробив би вказівник; ти знаєш краще - і кажеш це.

## 8. Мова анотацій

Повний набір анотацій запиту - всі вони є рядками-коментарями між
заголовком `-- name:` і тілом SQL.

### -- name: <Name> :команда

Починає запит. `<Name>` мусить бути експортованим Go-ідентифікатором,
унікальним у межах пакета. Команда визначає форму методу:

| Команда | Згенерована сигнатура | Для чого |
|---|---|---|
| `:one` | `(T, error)`; `sql.ErrNoRows`, коли нічого нема | вибірка за ключем, INSERT ... RETURNING |
| `:many` | `([]T, error)` | списки |
| `:iter` | `iter.Seq2[T, error]`; зупиняється на першій помилці | великі результати, стрімінг |
| `:exec` | `error` | виконав-і-забув |
| `:execrows` | `(int64, error)` - кількість зачеплених рядків | UPDATE/DELETE, де кількість важлива |

`T` вище живе за трьома правилами: вибірка **повного набору колонок**
таблиці повертає її модель (`Note`); **проєкція** повертає структуру
запиту (`SearchNotesRow`); **одна колонка** повертає голе значення
(`count(*)` дає `int64`).

### Doc-коментарі

Кожен `--` рядок, що не є анотацією з перелічених нижче, стає
godoc-реченням згенерованого методу. Пиши як godoc: третя особа, з
дієслова ("Returns...", "Inserts...") - ім'я методу pgc допише сам.

### -- param: $N <ім'я>

Називає один аргумент `$N`, коли SQL не дає придатної підказки.
Виведення покриває звичні форми - `col = $1` (з будь-якого боку),
`col IN ($1)`, `col = ANY($1)` і `$1 = ANY(col)`, `INSERT ... VALUES`
позиційно, `LIMIT`/`OFFSET` - а все інше стає `arg1`, `arg2`, ... доки не
назвеш.

### -- override: <колонка|$N> [go-тип] [notnull|nullable]

Поправляє одну колонку результату чи один параметр, коли каталог не може
знати краще:

```sql
-- override: total int64 notnull        -- вираз: примусити NOT NULL
-- override: avatar_url nullable        -- перемкнути лише nullability
-- override: $3 *time.Time              -- параметр може бути NULL
-- override: $1 github.com/google/uuid.UUID   -- тип з іншого модуля
```

Явний Go-тип береться дослівно, разом із його nullability. Тип з іншого
модуля пишеться повним import-шляхом; імпорт додається у згенерований
файл, а тип має, як звично, реалізовувати `sql.Scanner`/`driver.Valuer`.
Назвати колонку чи параметр, яких у стейтменті немає - помилка: одруківки
не проходять мовчки.

### -- embed: <таблиця> [as <Поле>]

Коли запит доджойнює цілий рядок іншої таблиці, вкладає його як модель
тієї таблиці замість розпластування:

```sql
-- name: NotesWithAuthor :many
-- embed: authors as Author
SELECT n.id, n.title, a.id, a.name, a.email
FROM notes n
JOIN authors a ON a.id = n.author_id;
```

```go
type NotesWithAuthorRow struct {
	ID     int64
	Title  string
	Author Author
}
```

Анотація збігається з першим суцільним прогоном колонок результату, який
точно дорівнює повному списку колонок таблиці, по порядку - вибирай
колонки таблиці разом (`a.*` це робить). Повторюй анотацію, щоб вкласти
кілька таблиць; іменуй поля через `as` - обов'язково, коли вкладаєш ту
саму таблицю двічі.

## 9. Конфігурація генерації: pgc.json

Створи `pgc.json` у корені проєкту:

```json
{
  "queries": "queries",
  "out": "internal/db",
  "package": "db",
  "json_tags": true,
  "rename": {
    "notes": "Note"
  }
}
```

Кожен ключ, включно з тими, які цей туторіал лишає за замовчуванням:

| Ключ | Дефолт | Значення |
|---|---|---|
| `queries` | `queries` | тека з `.sql` файлами |
| `out` | `internal/db` | куди пишеться згенерований пакет |
| `package` | base від `out` | ім'я згенерованого пакета |
| `nullable` | `pointer` | nullable-колонки як `*T`; `sqlnull` дає `sql.Null[T]` |
| `json_tags` | `false` | додати теги `` `json:"column_name"` `` до структур |
| `interface` | `false` | ще й емітити інтерфейс `Querier` для test double |
| `types` | `{}` | заміни Go-типу за PostgreSQL-типом, напр. `{"uuid": "string"}` |
| `rename` | `{}` | таблиця → ім'я структури, напр. `{"notes": "Note"}`; можна зі схемою |

Запис `rename` важливіший, ніж здається: pgc ніколи не вгадує однину,
тож без нього таблиця `notes` стане структурою `Notes`.

## 10. Генерація

```sh
pgc generate
```

```
internal/db/db.go
internal/db/models.go
internal/db/pgarray.go
internal/db/notes.sql.go
```

Екскурсія:

- **db.go** - плюмбінг: інтерфейс `DBTX` (його задовольняють і `*sql.DB`,
  і `*sql.Tx`), структура `Queries`, `New` і `WithTx`.
- **models.go** - enum `NoteStatus` з константами і структура `Note`,
  типізована з живої схеми (`Tags []string`, `CreatedAt time.Time`,
  json-теги на місці).
- **pgarray.go** - з'явився, бо схема використовує масиви: маленькі
  адаптери текстового формату масивів PostgreSQL; напряму ти їх не
  викликаєш.
- **notes.sql.go** - метод на кожен запит, з твоїми doc-коментарями і
  явними викликами `Scan`. Відкрий його - він написаний, щоб його читали.

Одна річ, якої ця схема не показує: **nullable**-колонка. Тут усі колонки
`NOT NULL`, тож усі поля - звичайні значення; nullable `text` вийшов би
`*string` (або `sql.Null[string]` з `"nullable": "sqlnull"`). Повна
таблиця мапінгу - в [DOC.UK.md](DOC.UK.md#мапінг-типів).

Перезапускай `pgc generate` щоразу, як змінюється схема чи запити; файли
перезаписуються детерміновано, тож `git diff` показує рівно те, що
змінилося.

## 11. Використання згенерованого коду

Згенерований пакет говорить звичайним `database/sql`, тож програмі
потрібен драйвер для нього - будь-який PostgreSQL-драйвер,
зареєстрований у `database/sql`, працює однаково. Туторіал бере один з
поширених:

```sh
go get github.com/lib/pq
```

Створи `main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"

	"example.com/notes/internal/db"

	_ "github.com/lib/pq"
)

func main() {
	ctx := context.Background()

	sqlDB, err := sql.Open("postgres", os.Getenv("PGC_DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()

	q := db.New(sqlDB)

	// Створюємо дві нотатки; дефолт enum-а робить їх чернетками.
	first, err := q.CreateNote(ctx, "Hello pgc", "The very first note.",
		[]string{"intro", "pgc"})
	if err != nil {
		log.Fatal(err)
	}
	if _, err := q.CreateNote(ctx, "Second", "", []string{"pgc"}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("created #%d %q status=%s tags=%v\n",
		first.ID, first.Title, first.Status, first.Tags)

	// Публікуємо першу; :execrows повідомляє кількість зачеплених.
	changed, err := q.PublishNote(ctx, first.ID)
	if err != nil || changed != 1 {
		log.Fatal("publish: ", err, changed)
	}

	// Масиви працюють і в запитах: $1 = ANY(tags).
	tagged, err := q.SearchByTag(ctx, "intro")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("tagged %q: %d note(s)\n", "intro", len(tagged))

	// Скаляр повертається голим значенням.
	total, err := q.CountNotes(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("total notes:", total)

	// Стрімимо без матеріалізації всього списку.
	for note, err := range q.IterNotes(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  #%d %s [%s]\n", note.ID, note.Title, note.Status)
	}

	// Те саме значення Queries працює всередині транзакції.
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	if err := q.WithTx(tx).DeleteNote(ctx, first.ID); err != nil {
		log.Fatal(err)
	}
	if err := tx.Rollback(); err != nil { // передумали
		log.Fatal(err)
	}
	if _, err := q.GetNote(ctx, first.ID); err != nil {
		log.Fatal("rollback мав зберегти нотатку: ", err)
	}
	fmt.Println("rollback kept the note, as a rollback should")

	// А sql.ErrNoRows - рівно те, що обіцяє документація.
	if _, err := q.GetNote(ctx, 99999); errors.Is(err, sql.ErrNoRows) {
		fmt.Println("missing note reports sql.ErrNoRows")
	}
}
```

Запусти:

```sh
go run .
```

```
created #1 "Hello pgc" status=draft tags=[intro pgc]
tagged "intro": 1 note(s)
total notes: 2
  #1 Hello pgc [published]
  #2 Second [draft]
rollback kept the note, as a rollback should
missing note reports sql.ErrNoRows
```

## 12. Цикл змін

Фінальна структура проєкту, для орієнтації:

```
notes/
├── docker-compose.yaml
├── migrations/001_init.sql
├── queries/notes.sql
├── pgc.json
├── internal/db/          <- згенероване, руками не редагується
├── main.go
├── go.mod
└── go.sum
```

Хто є джерелом правди для чого: **міграції** визначають базу; **жива
база** визначає типи, які генерує pgc; **queries/*.sql** визначають
поверхню API; `internal/db/` - завжди похідний артефакт. Коли вони
розходяться, цикл нижче їх вирівнює.

Зміна таблиці:

```sh
$EDITOR migrations/002_add_author.sql   # 1. новий файл міграції
pgc migrate                             # 2. застосувати
pgc generate                            # 3. перегенерувати
go build ./...                          # 4. компілятор покаже кожен наслідок
```

Зміна чи додавання запиту - той самий цикл без перших двох кроків.
Помилки компіляції в кроці 4 - це фіча, а не проблема: кожне місце
виклику, що більше не відповідає схемі, показане до будь-якого запуску.

## 13. Чесність у CI

Дві команди не дають згенерованому коду і схемі розійтися:

```sh
pgc check              # компілює кожен запит проти БД, нічого не пише
pgc generate && git diff --exit-code   # падає, якщо закомічений код застарів
```

У CI підніми одноразовий PostgreSQL-сервіс і запінь версії - і pgc, і Go:

```sh
go install github.com/goloop/pgc@v0.3.0
pgc migrate
pgc generate
git diff --exit-code
```

Два паралельні CI-джоби не влаштують гонку міграцій: `pgc migrate` тримає
advisory-лок PostgreSQL на весь запуск, тож другий джоб чекає.

## 14. Розв'язання проблем

Спершу як читати помилку pgc: проблеми часу генерації приходять як
`queries/notes.sql:12: GetNote: <що і де>` - файл, рядок заголовка
`-- name:` та ім'я запиту вказують на SQL, який треба виправити; скарга
сервера додатково несе позицію символа всередині стейтмента
(`position 8`). Помилки часу виконання йдуть від `database/sql`, як
звично.

| Симптом | Причина | Лікування |
|---|---|---|
| `connection refused` | контейнер не піднятий або порт не той | `docker compose ps`; порт у URL має збігатися з лівою частиною мапінгу `ports:` |
| `password authentication failed` | облікові дані URL відрізняються від environment compose | звір `POSTGRES_USER`/`POSTGRES_PASSWORD` з URL |
| `relation "notes" does not exist` | міграції не застосовані до цієї бази | `pgc migrate`; перевір, що база в URL та сама |
| `no database url` | `PGC_DATABASE_URL` не експортований у цій оболонці | повтори `export` із кроку 4 |
| `unsupported PostgreSQL type "xxx"` | тип колонки без дефолтного мапінгу | додай `"types": {"xxx": "string"}` у pgc.json або override на колонку |
| `a result column has no name` | вираз без аліаса | дай його: `count(*) AS total` |
| `generated name X collides` | дві таблиці мапляться на одне ім'я структури | додай запис `rename`, за потреби зі схемою |
| `embed ...: no remaining run` | колонки вкладеної таблиці не суцільні чи неповні | вибери їх разом і повністю: `a.*` |
| `go install` не бачить свіжу версію | module proxy ще не проіндексував тег | `GOPROXY=direct go install github.com/goloop/pgc@latest` |
| `pgc: command not found` після go install | `~/go/bin` нема в PATH | `export PATH="$PATH:$(go env GOPATH)/bin"` |

Коли туторіал стане тісним, повний довідник - **[DOC.UK.md](DOC.UK.md)**:
усі ключі конфігурації, повний мапінг типів, правила nullability, enum-и,
масиви, вкладення і CI-рецепт докладніше.

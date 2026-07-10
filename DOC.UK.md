# pgc - довідник

Повний довідник `pgc`: робочий процес, анотації query-файлів, конфігурація,
мапінг типів і анатомія згенерованого коду.

Англійська версія: **[DOC.md](DOC.md)**.

## Зміст

- [Ментальна модель](#ментальна-модель)
- [Робочий процес](#робочий-процес)
- [Query-файли](#query-файли)
- [Команди](#команди)
- [Overrides](#overrides)
- [Конфігурація](#конфігурація)
- [Мапінг типів](#мапінг-типів)
- [Nullability](#nullability)
- [Enum-и](#enum-и)
- [Згенерований код](#згенерований-код)
- [Рецепт для CI](#рецепт-для-ci)
- [Межі](#межі)

## Ментальна модель

pgc не парсить SQL. Кожен запит він надсилає у твою базу розробки
повідомленнями Parse/Describe wire-протоколу - стейтмент готується, але
ніколи не виконується - і сервер повідомляє точний тип кожного параметра
`$N` і кожної колонки результату. Nullability та мітки enum-ів добираються
із системних каталогів через те саме з'єднання. База, проти якої ти
розробляєш - єдине джерело правди, тож те, що згенерувалося, і є те, що
працюватиме.

Ціна цієї точності - жива база на момент генерації; одноразовий локальний
контейнер повністю це закриває. Винагорода - підтримується будь-який
стейтмент, який приймає твій сервер, і типи ніколи не вгадуються.

## Робочий процес

```
myapp/
├── migrations/          твої міграції, будь-який інструмент
├── queries/
│   └── users.sql        анотовані запити
├── internal/db/         згенерований пакет - не редагується
└── pgc.json             необов'язкова конфігурація
```

Цикл: застосувати міграції до dev-бази, запустити `pgc generate`,
закомітити результат. Згенеровані файли мають заголовок `// Code generated
by pgc. DO NOT EDIT.` і призначені для рев'ю та версіювання, як будь-який
інший код.

URL з'єднання береться з `PGC_DATABASE_URL` (або `DATABASE_URL`, або
прапорця `-d`) - ніколи з конфіг-файлу, щоб облікові дані не потрапляли в
репозиторій:

```
postgres://user:password@host:5432/dbname?sslmode=disable
```

`sslmode` приймає `disable`, `prefer` (за замовчуванням), `require` і
`verify-full`.

## Query-файли

Кожен запит у `.sql` файлі починається з анотації імені; рядки коментарів
між заголовком і SQL стають godoc-коментарем згенерованого методу:

```sql
-- name: GetUser :one
-- Returns a single user by primary key.
SELECT id, email, name, created_at
FROM users
WHERE id = $1;
```

Ім'я мусить бути експортованим Go-ідентифікатором, унікальним у межах
пакета. Коментарі всередині SQL-тіла не чіпаються й надсилаються серверу.

Імена параметрів виводяться із самого SQL: `id = $1` називає аргумент
`id`, `INSERT INTO t (a, b) VALUES ($1, $2)` зіставляється позиційно,
`LIMIT $1` стає `limit`. Коли виводити нема з чого, аргумент зветься
`argN`; дай йому краще ім'я явно:

```sql
-- param: $3 minimal_age
```

## Команди

| Команда | Форма методу |
|---|---|
| `:one` | `(T, error)` - `sql.ErrNoRows`, коли нічого не знайдено |
| `:many` | `([]T, error)` |
| `:iter` | `iter.Seq2[T, error]` - стрімить рядки без матеріалізації слайса |
| `:exec` | `error` |
| `:execrows` | `(int64, error)` - кількість зачеплених рядків |

`:iter` повертає range-over-func послідовність (Go 1.23+); ітерація
зупиняється на першій помилці:

```go
for u, err := range q.IterUsers(ctx) {
	if err != nil {
		return err
	}
	// використати u
}
```

## Overrides

Коли каталог не може знати краще - вирази, зовнішні джойни, екзотичні
типи - поправ одну колонку чи параметр на місці:

```sql
-- override: total int64 notnull
-- override: avatar_url nullable
-- override: $3 *time.Time
```

Форма: `-- override: <колонка|$N> [go-тип] [notnull|nullable]`. Явний
Go-тип береться дослівно (разом із його nullability); голе
`notnull`/`nullable` зберігає замаплений тип і перемикає лише обгортку.
Override, що називає колонку чи параметр, яких у стейтменті немає - це
помилка: одруківки не проходять мовчки.

## Конфігурація

`pgc.json` необов'язковий; дефолти описують звичну структуру:

```json
{
  "queries": "queries",
  "out": "internal/db",
  "package": "db",
  "nullable": "pointer",
  "types": {
    "uuid": "string",
    "numeric": "string"
  },
  "rename": {
    "users": "User"
  }
}
```

- **queries** - тека з `.sql` файлами.
- **out** - куди пишеться згенерований пакет.
- **package** - ім'я пакета; за замовчуванням - base від `out`.
- **nullable** - як рендеряться nullable-колонки: `pointer` (`*string`,
  дефолт) або `sqlnull` (`sql.Null[string]`).
- **types** - заміни Go-типу за іменем PostgreSQL-типу, застосовуються до
  nullability-обгортання.
- **rename** - ім'я таблиці → ім'я структури. Без запису береться
  CamelCase імені таблиці як є: pgc ніколи не вгадує однину, тож `users`
  буде `Users`, доки не скажеш `"users": "User"`.

## Мапінг типів

| PostgreSQL | Go (NOT NULL) | nullable, режим `pointer` |
|---|---|---|
| boolean | bool | *bool |
| smallint | int16 | *int16 |
| integer | int32 | *int32 |
| bigint | int64 | *int64 |
| real | float32 | *float32 |
| double precision | float64 | *float64 |
| text, varchar, char, name, citext | string | *string |
| bytea | []byte | []byte (nil = NULL) |
| date, timestamp, timestamptz | time.Time | *time.Time |
| json, jsonb | json.RawMessage | json.RawMessage (nil = NULL) |
| uuid, numeric, money | string | *string |
| time, timetz, interval | string | *string |
| inet, cidr, macaddr | string | *string |
| enum | іменований string-тип | *T |

Типи без природного відповідника в стандартній бібліотеці (uuid, numeric,
interval) за замовчуванням стають `string`; зміни це на рівні типу через
`types` або на рівні колонки через override. З `"nullable": "sqlnull"`
nullable-колонка стає `sql.Null[T]` замість `*T`.

## Nullability

- Колонка, що йде прямо з таблиці, бере каталожний `attnotnull`.
- Вираз (`count(*)`, `coalesce(...)`, обчислення) не має походження, тож
  він nullable за замовчуванням - додай `-- override: <ім'я> notnull`,
  коли знаєш краще.
- Параметри за замовчуванням не-nullable; передай тип-вказівник через
  override, коли NULL - валідний аргумент.
- **Зовнішні джойни**: каталог і далі повідомляє колонки приєднаної
  таблиці як NOT NULL, хоча джойн може дати NULL. pgc виявляє
  `LEFT/RIGHT/FULL JOIN` і друкує попередження з проханням явних
  `nullable`-override-ів для колонок зовнішньої сторони. Він попереджає,
  а не тихо бреше.

## Enum-и

PostgreSQL enum стає іменованим string-типом з константою на кожну мітку,
в порядку оголошення:

```go
// OrderStatus mirrors the PostgreSQL enum order_status.
type OrderStatus string

// The order_status values.
const (
	OrderStatusPending OrderStatus = "pending"
	OrderStatusPaid    OrderStatus = "paid"
)
```

Замапи enum у `types` (наприклад, на звичайний `string`), щоб відмовитися.

## Згенерований код

`db.go` несе плюмбінг, спільний для всіх запитів:

```go
type DBTX interface { /* ExecContext, QueryContext, QueryRowContext */ }

q := db.New(sqlDB)          // *sql.DB задовольняє DBTX
q.WithTx(tx).DeleteUser(..) // *sql.Tx теж задовольняє
```

`models.go` тримає по структурі на кожну таблицю, яку якийсь запит вибирає
повністю, з типами з каталогу. Форма результату йде за трьома правилами:

- повний набір колонок таблиці повертає модель (`User`);
- проєкція повертає структуру запиту (`SearchOrdersRow`);
- одна колонка повертає голе значення (`count(*)` дає `(int64, error)`).

До трьох параметрів лишаються позиційними (`GetUser(ctx, id int64)`);
чотири й більше стають структурою `XxxParams`. Сканування - завжди явні
виклики `row.Scan`: у рантаймі немає рефлексії і нема що конфігурувати.

## Рецепт для CI

```sh
docker run -d --name ci-pg -e POSTGRES_PASSWORD=ci -p 5432:5432 postgres:17-alpine
# тут застосувати міграції
export PGC_DATABASE_URL="postgres://postgres:ci@localhost:5432/postgres?sslmode=disable"
pgc generate
git diff --exit-code   # падає, коли закомічений код розійшовся
```

`pgc check` робить ту саму компіляцію без запису, коли потрібна лише
перевірка.

## Межі

pgc генерує біндинги запитів; він свідомо не ORM, не керує міграціями й сам
не виконує запити застосунку - згенерований код говорить звичайним
`database/sql`, і вибір драйвера лишається за тобою. Колонки-масиви та
кастомні Go-типи зі сторонніх пакетів поки не підтримуються.

// Пакет postgres реализует интерфейс site.Repository поверх PostgreSQL
// (драйвер pgx), а также подключение к базе и применение миграций.
// Только этот пакет знает SQL и коды ошибок PostgreSQL.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"sitewatch/internal/site"
	"sitewatch/migrations"
)

// uniqueViolation — код ошибки PostgreSQL (SQLSTATE) «нарушение уникальности».
// Возникает при вставке URL, который уже есть: колонка sites.url — UNIQUE.
const uniqueViolation = "23505"

// Connect создаёт пул соединений с базой по строке подключения dsn и
// проверяет, что база действительно доступна.
//
// Пул — один на всё приложение и безопасен для конкурентного использования:
// каждый запрос берёт свободное соединение и затем возвращает его в пул.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	// Подключение не должно висеть дольше 10 секунд. Здесь ctx получает новое
	// значение (это не затенение — та же переменная): исходный контекст
	// дальше не нужен.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// pgxpool.New открывает соединения лениво, поэтому сам по себе
	// не гарантирует, что база работает.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	// Ping — реальная проверка связи.
	if err = pool.Ping(ctx); err != nil {
		pool.Close() // не оставляем «висящий» пул при ошибке
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate применяет к базе все ещё не применённые миграции из migrations.FS
// (SQL-файлы встроены в исполняемый файл). Возвращает список применённых
// миграций — он пуст, если база уже в актуальном состоянии.
// Номер последней применённой миграции goose хранит в таблице goose_db_version.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]*goose.MigrationResult, error) {
	// goose работает со стандартным интерфейсом database/sql (*sql.DB),
	// а у нас пул pgx. OpenDBFromPool делает из пула объект *sql.DB.
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	// Up применяет недостающие миграции по порядку их номеров.
	results, err := provider.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return results, nil
}

// Repository — хранилище сайтов и проверок в PostgreSQL.
// Реализует интерфейс site.Repository (неявно — просто имея нужные методы).
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository создаёт репозиторий поверх уже открытого пула соединений.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// CreateSite вставляет сайт и возвращает его с назначенными базой id и
// created_at. Если такой URL уже есть — возвращает site.ErrAlreadyExists.
func (r *Repository) CreateSite(ctx context.Context, s site.Site) (site.Site, error) {
	// $1, $2 — плейсхолдеры: значения передаются отдельно от текста запроса,
	// это защищает от SQL-инъекций. RETURNING возвращает сгенерированные
	// значения сразу, без второго запроса.
	const q = `INSERT INTO sites (name, url) VALUES ($1, $2) RETURNING id, created_at`

	// s получен по значению — меняем свою копию и её же возвращаем.
	err := r.pool.QueryRow(ctx, q, s.Name, s.URL).Scan(&s.ID, &s.CreatedAt)
	if err != nil {
		// errors.As ищет в цепочке ошибку типа *pgconn.PgError и записывает
		// её в pgErr. Ошибку инфраструктуры (код PostgreSQL) переводим в
		// доменную — так верхние слои ничего не знают о базе данных.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return site.Site{}, site.ErrAlreadyExists
		}
		return site.Site{}, err
	}
	return s, nil
}

// ListSites возвращает все сайты, у каждого — самую свежую проверку
// (или nil в LastCheck, если проверок ещё не было). Всё — одним запросом.
func (r *Repository) ListSites(ctx context.Context) ([]site.Site, error) {
	// LEFT JOIN LATERAL: подзапрос выполняется для каждой строки sites и может
	// ссылаться на неё (s.id). Он берёт одну самую свежую проверку сайта.
	// LEFT — сайты без проверок тоже попадают в результат (с NULL в колонках
	// проверки). ON true — условие соединения уже внутри подзапроса.
	// Так решается проблема «N+1 запросов»: вместо запроса списка и отдельного
	// запроса на каждый сайт — один запрос. Индекс (site_id, checked_at DESC)
	// делает подзапрос быстрым.
	const q = `
		SELECT s.id, s.name, s.url, s.created_at,
		       c.status_code, c.latency_ms, c.error, c.checked_at
		FROM sites s
		LEFT JOIN LATERAL (
			SELECT status_code, latency_ms, error, checked_at
			FROM checks
			WHERE site_id = s.id
			ORDER BY checked_at DESC
			LIMIT 1
		) c ON true
		ORDER BY s.id`

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // строки обязательно закрывать, иначе соединение не вернётся в пул

	// Пустой срез, а не nil: в JSON он станет [], а nil-срез — null.
	// Клиенту удобнее всегда получать массив.
	sites := make([]site.Site, 0)
	for rows.Next() {
		var (
			s site.Site
			// Колонки из LEFT JOIN могут быть NULL, поэтому сканируются
			// в указатели: nil означает NULL.
			status    *int
			latency   *int64
			errText   *string
			checkedAt *time.Time
		)
		if err = rows.Scan(&s.ID, &s.Name, &s.URL, &s.CreatedAt, &status, &latency, &errText, &checkedAt); err != nil {
			return nil, err
		}
		// Если проверка нашлась, все её колонки не NULL (в таблице checks
		// они NOT NULL), и указатели можно безопасно разыменовать.
		if checkedAt != nil {
			s.LastCheck = &site.Check{
				SiteID:     s.ID,
				StatusCode: *status,
				LatencyMS:  *latency,
				Error:      *errText,
				CheckedAt:  *checkedAt,
			}
		}
		sites = append(sites, s)
	}
	// rows.Err() сообщает об ошибке, случившейся во время перебора строк
	// (например, оборвалось соединение). Без этой проверки её легко потерять.
	return sites, rows.Err()
}

// DeleteSite удаляет сайт по id. Если такого сайта нет — site.ErrNotFound.
// Проверки сайта база удаляет сама (ON DELETE CASCADE).
func (r *Repository) DeleteSite(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM sites WHERE id = $1`, id)
	if err != nil {
		return err
	}
	// Ноль затронутых строк — значит, удалять было нечего.
	if tag.RowsAffected() == 0 {
		return site.ErrNotFound
	}
	return nil
}

// SaveChecks сохраняет все результаты прогона ОДНИМ запросом (batch insert).
// Проверки сайтов, удалённых, пока шёл прогон, молча пропускаются.
func (r *Repository) SaveChecks(ctx context.Context, checks []site.Check) error {
	// unnest превращает несколько массивов в набор строк (i-й элемент каждого
	// массива — одна строка), а INSERT ... SELECT вставляет их все сразу:
	// 100 проверок — один запрос к базе вместо 100.
	// WHERE EXISTS отбрасывает проверки удалённых сайтов: иначе нарушился бы
	// внешний ключ, и вставка упала бы целиком.
	const q = `
		INSERT INTO checks (site_id, status_code, latency_ms, error, checked_at)
		SELECT t.site_id, t.status_code, t.latency_ms, t.error, t.checked_at
		FROM unnest($1::bigint[], $2::int[], $3::bigint[], $4::text[], $5::timestamptz[])
		     AS t(site_id, status_code, latency_ms, error, checked_at)
		WHERE EXISTS (SELECT 1 FROM sites s WHERE s.id = t.site_id)`

	// Раскладываем срез структур в пять параллельных срезов — по одному
	// на колонку («из строк в столбцы»).
	n := len(checks)
	var (
		siteIDs   = make([]int64, n)
		statuses  = make([]int32, n) // колонка INT — 32 бита, поэтому int32
		latencies = make([]int64, n)
		errs      = make([]string, n)
		times     = make([]time.Time, n)
	)
	for i, c := range checks {
		// Множественное присваивание: пять значений за одну операцию.
		siteIDs[i], statuses[i], latencies[i], errs[i], times[i] =
			c.SiteID, int32(c.StatusCode), c.LatencyMS, c.Error, c.CheckedAt
	}

	_, err := r.pool.Exec(ctx, q, siteIDs, statuses, latencies, errs, times)
	return err
}

// ListChecks возвращает последние limit проверок сайта, новые первыми.
// Если сайта нет — site.ErrNotFound.
func (r *Repository) ListChecks(ctx context.Context, siteID int64, limit int) ([]site.Check, error) {
	// Сначала проверяем, существует ли сайт. Без этого для несуществующего
	// сайта вернулся бы пустой список — и клиент не отличил бы его от сайта,
	// у которого просто ещё нет проверок. А так он получит 404.
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sites WHERE id = $1)`, siteID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, site.ErrNotFound
	}

	const q = `
		SELECT site_id, status_code, latency_ms, error, checked_at
		FROM checks
		WHERE site_id = $1
		ORDER BY checked_at DESC
		LIMIT $2`

	rows, err := r.pool.Query(ctx, q, siteID, limit)
	if err != nil {
		return nil, err
	}
	// pgx.CollectRows — generic-помощник: перебирает строки, вызывает функцию
	// для каждой, собирает результаты в срез и сам закрывает rows.
	// Ошибка внутри замыкания названа scanErr, чтобы не скрыть внешнюю err.
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (site.Check, error) {
		var c site.Check
		scanErr := row.Scan(&c.SiteID, &c.StatusCode, &c.LatencyMS, &c.Error, &c.CheckedAt)
		return c, scanErr
	})
}

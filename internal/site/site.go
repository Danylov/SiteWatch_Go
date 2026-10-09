// Пакет site содержит доменную модель (типы предметной области) и
// бизнес-логику монитора. Он не зависит ни от базы данных, ни от HTTP:
// всё внешнее подключается через интерфейсы, объявленные в service.go.
package site

import (
	"errors"
	"time"
)

// Доменные ошибки (sentinel-ошибки, «ошибки-маркеры»).
// Это заранее созданные значения, с которыми сравнивают через errors.Is.
// Слой хранения переводит свои ошибки (например, коды PostgreSQL) в эти,
// а HTTP-слой в одном месте (httpapi/errors.go) переводит их в коды ответа.
var (
	ErrNotFound        = errors.New("site not found")                     // → 404
	ErrAlreadyExists   = errors.New("site with this URL already exists")  // → 409
	ErrCheckInProgress = errors.New("a check run is already in progress") // → 409
)

// Site — отслеживаемый сайт.
// Теги `json:"..."` задают имена полей в JSON-ответах API.
type Site struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
	// LastCheck — указатель, потому что значение необязательное:
	// nil означает «сайт ещё не проверялся» (в JSON будет null).
	LastCheck *Check `json:"last_check"`
}

// Check — результат одной проверки сайта.
type Check struct {
	SiteID     int64 `json:"site_id"`
	StatusCode int   `json:"status_code"` // HTTP-код; 0, если ответа не было
	LatencyMS  int64 `json:"latency_ms"`  // время ответа в миллисекундах
	// Error — текст сетевой ошибки или таймаута; пусто, если ответ получен.
	// omitempty: пустое поле не выводится в JSON.
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// Up сообщает, считать ли сайт доступным: ошибки нет и код ответа 2xx или 3xx.
// Ответ 404 или 500 — сайт ответил, но «нездоров», это DOWN.
//
// Получатель — значение (c Check), а не указатель: структура небольшая,
// а метод её только читает.
func (c Check) Up() bool {
	return c.Error == "" && c.StatusCode >= 200 && c.StatusCode < 400
}

// CreateInput — данные для добавления нового сайта.
// Отдельный тип, а не Site: при создании нет ни ID, ни CreatedAt —
// их назначает база данных.
type CreateInput struct {
	Name string
	URL  string
}

// RunSummary — итог одного прогона проверок.
type RunSummary struct {
	Total      int       `json:"total"`       // сколько сайтов проверено
	Up         int       `json:"up"`          // сколько доступны
	Down       int       `json:"down"`        // сколько недоступны
	DurationMS int64     `json:"duration_ms"` // длительность прогона
	FinishedAt time.Time `json:"finished_at"`
}

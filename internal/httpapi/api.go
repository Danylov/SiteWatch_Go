// Пакет httpapi открывает доступ к сервису через JSON REST API и раздаёт
// веб-интерфейс. Обработчики здесь «тонкие»: разобрать запрос → вызвать
// сервис → записать ответ. Вся логика — в site.Service.
package httpapi

// Директива для «go generate ./...»: генерирует мок SiteService в mocks_test.go.
//go:generate go tool mockgen -source=api.go -destination=mocks_test.go -package=httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"

	"sitewatch/internal/site"
)

// SiteService — то, что обработчикам нужно от слоя бизнес-логики.
// Объявлен здесь, у потребителя; реализуется *site.Service, а в тестах —
// сгенерированным MockSiteService.
type SiteService interface {
	CreateSite(ctx context.Context, in site.CreateInput) (site.Site, error)
	ListSites(ctx context.Context) ([]site.Site, error)
	DeleteSite(ctx context.Context, id int64) error
	ListChecks(ctx context.Context, siteID int64, limit int) ([]site.Check, error)
	RunChecks(ctx context.Context) (site.RunSummary, error)
	LastRun() (site.RunSummary, bool)
}

// API хранит зависимости обработчиков. Все обработчики — методы *API,
// поэтому им доступны svc и log без глобальных переменных.
type API struct {
	svc SiteService
	log *slog.Logger
}

// NewRouter регистрирует все маршруты и оборачивает их в middleware.
// Возвращает итоговый http.Handler для http.Server.
//
// Стандартный ServeMux начиная с Go 1.22 понимает метод в шаблоне
// ("GET /api/sites") и параметры пути ("{id}"), поэтому сторонний роутер
// не нужен. Если путь есть, а метод другой, ServeMux сам ответит 405;
// неизвестный путь — 404.
//
// static — файлы веб-интерфейса; nil отключает их раздачу (так в тестах).
func NewRouter(svc SiteService, log *slog.Logger, static fs.FS) http.Handler {
	a := &API{svc: svc, log: log}

	mux := http.NewServeMux()
	// a.handle(...) превращает наш обработчик, возвращающий error,
	// в стандартный http.HandlerFunc (см. errors.go).
	mux.HandleFunc("GET /api/sites", a.handle(a.listSites))
	mux.HandleFunc("POST /api/sites", a.handle(a.createSite))
	mux.HandleFunc("DELETE /api/sites/{id}", a.handle(a.deleteSite))
	mux.HandleFunc("GET /api/sites/{id}/checks", a.handle(a.listChecks))
	mux.HandleFunc("POST /api/checks/run", a.handle(a.runChecks))
	mux.HandleFunc("GET /api/checks/last", a.handle(a.lastRun))
	// Проверка «жив ли сервис» для мониторинга и оркестраторов (Kubernetes и т.п.).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	if static != nil {
		// "GET /" — самый общий шаблон: срабатывает, если не подошёл ни один
		// более конкретный. FileServerFS (Go 1.22+) раздаёт файлы из fs.FS.
		mux.Handle("GET /", http.FileServerFS(static))
	}

	// Цепочка middleware: запрос проходит recoverer → logRequests → mux.
	// recoverer снаружи, чтобы перехватить панику где угодно ниже.
	return recoverer(log, logRequests(log, mux))
}

// createSiteRequest — тело запроса POST /api/sites (DTO, объект передачи данных).
//
// Отдельный тип, а не site.Site: клиент не должен иметь возможности передать
// id или created_at, а правила валидации (теги validate) — забота HTTP-слоя.
type createSiteRequest struct {
	Name string `json:"name" validate:"required,max=100"`
	URL  string `json:"url" validate:"required,http_url,max=2048"`
}

// createSite обрабатывает POST /api/sites: разбирает и валидирует JSON,
// создаёт сайт и отвечает 201 Created с JSON созданного сайта.
//
// Все обработчики не пишут ошибки в ответ сами, а возвращают их —
// ответ с ошибкой формирует одна функция writeError.
func (a *API) createSite(w http.ResponseWriter, r *http.Request) error {
	var req createSiteRequest
	// &req — указатель: decodeJSON должна заполнить именно эту переменную.
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}

	// r.Context() — контекст запроса: если клиент закроет соединение,
	// он отменится, и запрос к базе тоже прервётся.
	created, err := a.svc.CreateSite(r.Context(), site.CreateInput{Name: req.Name, URL: req.URL})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, created)
	return nil
}

// listSites обрабатывает GET /api/sites: отдаёт все сайты с последними проверками.
func (a *API) listSites(w http.ResponseWriter, r *http.Request) error {
	sites, err := a.svc.ListSites(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, sites)
	return nil
}

// deleteSite обрабатывает DELETE /api/sites/{id}: удаляет сайт и отвечает
// 204 No Content (успех без тела ответа).
func (a *API) deleteSite(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if err = a.svc.DeleteSite(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// listChecks обрабатывает GET /api/sites/{id}/checks?limit=N:
// отдаёт историю проверок сайта, новые первыми.
func (a *API) listChecks(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}

	// 0 означает «limit не указан» — значение по умолчанию подставит сервис.
	// Если же клиент передал limit, он должен быть целым числом >= 1.
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			return badRequest("limit must be a positive integer")
		}
	}

	checks, err := a.svc.ListChecks(r.Context(), id, limit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, checks)
	return nil
}

// runChecks обрабатывает POST /api/checks/run: запускает проверку всех сайтов,
// ЖДЁТ её окончания и отвечает итогом прогона. Если прогон уже идёт,
// сервис вернёт ErrCheckInProgress, и клиент получит 409.
func (a *API) runChecks(w http.ResponseWriter, r *http.Request) error {
	summary, err := a.svc.RunChecks(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, summary)
	return nil
}

// lastRun обрабатывает GET /api/checks/last: отдаёт итог последнего прогона
// или 204 No Content, если прогонов ещё не было.
// Параметр запроса не нужен, поэтому назван «_».
func (a *API) lastRun(w http.ResponseWriter, _ *http.Request) error {
	summary, ok := a.svc.LastRun()
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	writeJSON(w, http.StatusOK, summary)
	return nil
}

// pathID достаёт параметр {id} из пути запроса и проверяет, что это
// положительное целое число. Для "abc", "0", "-1" возвращает ошибку 400.
func pathID(r *http.Request) (int64, error) {
	// PathValue (Go 1.22+) возвращает значение параметра из шаблона маршрута.
	// ParseInt(s, 10, 64): десятичная система, результат помещается в int64.
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, badRequest("id must be a positive integer")
	}
	return id, nil
}

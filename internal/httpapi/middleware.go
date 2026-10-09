package httpapi

// Middleware — функции, которые оборачивают обработчик (next) и добавляют
// общее поведение до и после него: логирование, перехват паник и т.п.
// Форма: func(next http.Handler) http.Handler (здесь ещё передаётся логгер).

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// statusRecorder — обёртка над http.ResponseWriter, которая запоминает код
// ответа, записанный следующим обработчиком (нужен для лога).
//
// Поле http.ResponseWriter встроено без имени (embedding): все его методы
// (Header, Write, ...) автоматически доступны у statusRecorder, а
// переопределён только WriteHeader.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader сохраняет код ответа и передаёт вызов настоящему ResponseWriter.
func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap возвращает исходный ResponseWriter. Через этот метод
// http.ResponseController (Go 1.20+) добирается сквозь обёртку до настоящего
// объекта, чтобы вызвать Flush, SetWriteDeadline и т.п.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// logRequests пишет в лог каждый запрос: метод, путь, код ответа и длительность.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	// http.HandlerFunc — приведение обычной функции к интерфейсу http.Handler.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// По умолчанию 200: если обработчик вызовет только Write без
		// WriteHeader, Go сам отправит 200, и наш WriteHeader не вызовется.
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r) // вызываем следующий обработчик с подменённым writer

		log.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start),
		)
	})
}

// recoverer перехватывает панику в обработчике: пишет её со стеком вызовов
// в лог и отвечает клиенту 500, вместо того чтобы молча оборвать соединение.
func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// recover() работает только внутри отложенной (defer) функции и только
		// в той горутине, где случилась паника. Каждый запрос обрабатывается
		// в своей горутине, поэтому middleware и стоит на пути каждого запроса.
		defer func() {
			if v := recover(); v != nil {
				// http.ErrAbortHandler — особая паника, которой обработчик
				// сознательно обрывает ответ. Её пробрасываем дальше, чтобы
				// сервер корректно закрыл соединение.
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("panic recovered", "panic", v, "stack", string(debug.Stack()))
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

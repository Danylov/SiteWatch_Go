package httpapi

// Централизованная обработка ошибок HTTP-слоя: типы ошибок и единственное
// место (writeError), где любая ошибка превращается в HTTP-ответ.
// Благодаря этому формат ошибок одинаков во всём API, а внутренние детали
// никогда не уходят клиенту.

import (
	"errors"
	"net/http"

	"sitewatch/internal/site"
)

// requestError — ошибка из-за некорректного запроса клиента
// (битый JSON, неверный id и т.п.). Хранит HTTP-код и сообщение.
type requestError struct {
	status int
	msg    string
}

// Error возвращает текст ошибки. Благодаря этому методу *requestError
// реализует встроенный интерфейс error.
func (e *requestError) Error() string { return e.msg }

// badRequest создаёт ошибку «400 Bad Request» с сообщением для клиента.
// Возвращает интерфейс error, а не *requestError, — так её можно сразу
// вернуть из обработчика.
func badRequest(msg string) error {
	return &requestError{status: http.StatusBadRequest, msg: msg}
}

// validationError — ошибка валидации полей запроса.
// fields: «имя поля в JSON → сообщение», например {"url": "is required"}.
type validationError struct {
	fields map[string]string
}

// Error возвращает общий текст; подробности по полям клиент получит
// в поле "fields" ответа.
func (e *validationError) Error() string { return "validation failed" }

// errorResponse — единый формат JSON-ответа с ошибкой:
// {"error": "...", "fields": {...}}. omitempty скрывает fields, если map пуста.
type errorResponse struct {
	Error  string            `json:"error"`
	Fields map[string]string `json:"fields,omitempty"`
}

// handlerFunc — обработчик HTTP, который возвращает ошибку, а не пишет её в
// ответ сам. Похож на стандартный http.HandlerFunc, но с результатом error.
// Все ошибки превращаются в ответы в одном месте — (*API).handle.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// handle — адаптер: превращает наш handlerFunc в стандартный http.HandlerFunc.
// Возвращает замыкание, которое вызывает fn и, если та вернула ошибку,
// передаёт её в writeError.
func (a *API) handle(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			a.writeError(w, r, err)
		}
	}
}

// writeError выбирает HTTP-код по типу ошибки и пишет JSON-ответ.
//
// errors.As ищет в цепочке обёрток ошибку определённого ТИПА и записывает
// её в переменную (поэтому передаётся указатель на переменную: &valErr).
// errors.Is сравнивает с конкретным ЗНАЧЕНИЕМ (sentinel-ошибкой).
// Обе функции разворачивают обёртки, созданные fmt.Errorf("...: %w", err),
// поэтому сервис может добавлять к ошибкам контекст, не ломая это сопоставление.
func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		reqErr *requestError
		valErr *validationError
	)

	switch {
	case errors.As(err, &valErr):
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: valErr.Error(), Fields: valErr.fields})
	case errors.As(err, &reqErr):
		writeJSON(w, reqErr.status, errorResponse{Error: reqErr.msg})
	case errors.Is(err, site.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: site.ErrNotFound.Error()})
	case errors.Is(err, site.ErrAlreadyExists):
		writeJSON(w, http.StatusConflict, errorResponse{Error: site.ErrAlreadyExists.Error()})
	case errors.Is(err, site.ErrCheckInProgress):
		writeJSON(w, http.StatusConflict, errorResponse{Error: site.ErrCheckInProgress.Error()})
	default:
		// Неизвестная ошибка — это сбой на нашей стороне (например, упала БД).
		// Подробности пишем только в лог: клиенту их показывать нельзя
		// (там могут быть имена таблиц, адреса серверов и т.п.).
		a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
	}
}

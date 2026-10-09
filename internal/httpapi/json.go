package httpapi

// Чтение JSON из тела запроса (с ограничениями и понятными ошибками),
// валидация по тегам validate и запись JSON-ответов.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
)

// maxBodyBytes — максимальный размер тела запроса: 1 << 20 (сдвиг единицы
// на 20 бит) = 1 048 576 байт = 1 МБ.
const maxBodyBytes = 1 << 20

// validate — один валидатор на весь пакет. Он кэширует сведения о структурах
// (их получение через reflection дорого), поэтому создавать его на каждый
// запрос невыгодно. Переменная уровня пакета инициализируется один раз
// при старте программы.
var validate = newValidator()

// newValidator создаёт и настраивает валидатор.
func newValidator() *validator.Validate {
	// WithRequiredStructEnabled — рекомендуемый режим, в котором тег required
	// работает и для вложенных структур.
	v := validator.New(validator.WithRequiredStructEnabled())
	// По умолчанию валидатор сообщает имена Go-полей ("URL"), а клиенту нужны
	// имена из JSON ("url"). Эта функция берёт тег json через reflect и
	// отрезает опции после запятой: `json:"error,omitempty"` → "error".
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" { // json:"-" — поле не участвует в JSON
			return ""
		}
		return name
	})
	return v
}

// decodeJSON читает тело запроса в dst и валидирует результат.
// dst должен быть указателем на структуру — иначе заполнять будет нечего.
// Возвращает *requestError (400) при проблемах с JSON и *validationError (422)
// при ошибках в значениях полей.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	// Ограничиваем размер тела: защита от огромных запросов.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	// Лишнее поле ("admin": true) вызовет ошибку, а не будет тихо проигнорировано.
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return badRequest(describeJSONError(err))
	}
	// В теле должен быть ровно один JSON-объект. Повторный Decode обязан
	// вернуть io.EOF («данных больше нет»); тело вида {...}{...} отклоняем.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return badRequest("request body must contain a single JSON object")
	}

	if err := validate.Struct(dst); err != nil {
		var verrs validator.ValidationErrors
		if errors.As(err, &verrs) {
			return &validationError{fields: fieldMessages(verrs)}
		}
		// Другие ошибки валидатора означают ошибку программиста
		// (например, передали не структуру) — это 500, а не вина клиента.
		return err
	}
	return nil
}

// describeJSONError переводит ошибку пакета encoding/json в понятное
// клиенту сообщение.
func describeJSONError(err error) string {
	var (
		syntaxErr   *json.SyntaxError
		typeErr     *json.UnmarshalTypeError
		maxBytesErr *http.MaxBytesError
	)
	switch {
	case errors.As(err, &syntaxErr):
		// Offset — позиция (в байтах), где разбор споткнулся.
		return fmt.Sprintf("malformed JSON at position %d", syntaxErr.Offset)
	case errors.Is(err, io.ErrUnexpectedEOF):
		// JSON оборвался посередине: {"name":
		return "malformed JSON"
	case errors.As(err, &typeErr):
		// Значение не того типа: {"name": 1} вместо строки.
		return fmt.Sprintf("field %q must be of type %s", typeErr.Field, typeErr.Type)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// Для неизвестного поля в encoding/json нет отдельного типа ошибки,
		// поэтому приходится проверять текст. Известное ограничение библиотеки.
		return "unknown field " + strings.TrimPrefix(err.Error(), "json: unknown field ")
	case errors.Is(err, io.EOF):
		return "request body is empty"
	case errors.As(err, &maxBytesErr):
		return fmt.Sprintf("request body must not exceed %d bytes", maxBytesErr.Limit)
	default:
		return "invalid request body"
	}
}

// fieldMessages превращает список ошибок валидатора в map
// «имя поля → понятное сообщение» для ответа клиенту.
func fieldMessages(verrs validator.ValidationErrors) map[string]string {
	// Ёмкость map задаётся заранее — известно, сколько будет элементов.
	out := make(map[string]string, len(verrs))
	for _, fe := range verrs {
		var msg string
		switch fe.Tag() { // Tag — какое правило из тега validate не прошло
		case "required":
			msg = "is required"
		case "max":
			msg = fmt.Sprintf("must be at most %s characters", fe.Param()) // Param — значение из max=100
		case "http_url":
			msg = "must be a valid http(s) URL"
		default:
			msg = "is invalid"
		}
		// Field возвращает имя из JSON благодаря RegisterTagNameFunc выше.
		out[fe.Field()] = msg
	}
	return out
}

// writeJSON пишет ответ: заголовок Content-Type, код статуса и тело в JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	// Порядок важен: заголовки задаются ДО WriteHeader — после отправки
	// кода статуса изменить их уже нельзя.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Ошибку Encode игнорируем: статус уже отправлен, исправить ответ невозможно
	// (обычно это значит, что клиент отключился).
	_ = json.NewEncoder(w).Encode(v)
}

package httpapi

// Тесты HTTP-слоя без сети: httptest.NewRequest создаёт запрос,
// httptest.NewRecorder записывает ответ, а роутер вызывается напрямую
// через ServeHTTP. Сервис заменён сгенерированным MockSiteService.

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"sitewatch/internal/site"
)

// newTestServer собирает роутер с моком сервиса, «тихим» логгером и без
// веб-интерфейса (static = nil). Возвращает роутер и мок для задания ожиданий.
func newTestServer(t *testing.T) (http.Handler, *MockSiteService) {
	t.Helper()
	svc := NewMockSiteService(gomock.NewController(t))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRouter(svc, log, nil), svc
}

// do выполняет запрос к обработчику h и возвращает записанный ответ.
func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeBody разбирает JSON-ответ в значение типа T.
// Это generic-функция: тип указывается при вызове, например decodeBody[site.Site](t, rec).
func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(rec.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

// TestCreateSite_Success: корректный запрос доходит до сервиса с нужными
// данными, ответ — 201 и созданный сайт.
func TestCreateSite_Success(t *testing.T) {
	h, svc := newTestServer(t)

	svc.EXPECT().
		CreateSite(gomock.Any(), site.CreateInput{Name: "Go", URL: "https://go.dev"}).
		Return(site.Site{ID: 42, Name: "Go", URL: "https://go.dev"}, nil)

	rec := do(h, http.MethodPost, "/api/sites", `{"name":"Go","url":"https://go.dev"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusCreated, rec.Body)
	}
	if got := decodeBody[site.Site](t, rec); got.ID != 42 {
		t.Errorf("ID = %d, want 42", got.ID)
	}
}

// TestCreateSite_BadInput — табличный тест плохого ввода.
// Некорректный ввод должен отклоняться ДО обращения к сервису: у мока нет
// ни одного ожидания, поэтому любой его вызов уронит тест.
func TestCreateSite_BadInput(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantFields []string // поля, для которых ожидаются ошибки (для 422)
		wantError  string   // подстрока в поле "error" ответа
	}{
		// Ошибки формата JSON → 400.
		{"malformed json", `{"name":`, http.StatusBadRequest, nil, "malformed JSON"},
		{"empty body", ``, http.StatusBadRequest, nil, "request body is empty"},
		{"wrong type", `{"name":1,"url":"https://go.dev"}`, http.StatusBadRequest, nil, `field "name"`},
		{"unknown field", `{"name":"Go","url":"https://go.dev","admin":true}`, http.StatusBadRequest, nil, "unknown field"},
		{"two objects", `{"name":"Go","url":"https://go.dev"}{}`, http.StatusBadRequest, nil, "single JSON object"},
		// JSON корректный, но значения не проходят валидацию → 422.
		{"missing fields", `{}`, http.StatusUnprocessableEntity, []string{"name", "url"}, ""},
		{"not a url", `{"name":"Go","url":"go.dev"}`, http.StatusUnprocessableEntity, []string{"url"}, ""},
		{"ftp url", `{"name":"Go","url":"ftp://go.dev"}`, http.StatusUnprocessableEntity, []string{"url"}, ""},
		{"name too long", `{"name":"` + strings.Repeat("a", 101) + `","url":"https://go.dev"}`, http.StatusUnprocessableEntity, []string{"name"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newTestServer(t)

			rec := do(h, http.MethodPost, "/api/sites", tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body)
			}

			resp := decodeBody[errorResponse](t, rec)
			// strings.Contains с пустой подстрокой всегда true — для случаев 422
			// текст ошибки не проверяется.
			if !strings.Contains(resp.Error, tt.wantError) {
				t.Errorf("error = %q, want it to contain %q", resp.Error, tt.wantError)
			}
			for _, f := range tt.wantFields {
				if _, ok := resp.Fields[f]; !ok {
					t.Errorf("expected an error for field %q, got %v", f, resp.Fields)
				}
			}
		})
	}
}

// TestErrorMapping проверяет перевод ошибок сервиса в HTTP-коды и то, что
// текст внутренней ошибки не утекает клиенту.
func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"not found", site.ErrNotFound, http.StatusNotFound},
		// Обёрнутая ошибка тоже должна распознаваться (errors.Is).
		{"wrapped not found", errors.Join(errors.New("ctx"), site.ErrNotFound), http.StatusNotFound},
		{"internal", errors.New("pq: connection reset"), http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, svc := newTestServer(t)
			svc.EXPECT().DeleteSite(gomock.Any(), int64(5)).Return(tt.err)

			rec := do(h, http.MethodDelete, "/api/sites/5", "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			// Подробности внутренней ошибки никогда не должны попадать к клиенту.
			if strings.Contains(rec.Body.String(), "pq:") {
				t.Errorf("internal error leaked: %s", rec.Body)
			}
		})
	}
}

// TestDeleteSite: успешное удаление отвечает 204 No Content.
func TestDeleteSite(t *testing.T) {
	h, svc := newTestServer(t)
	svc.EXPECT().DeleteSite(gomock.Any(), int64(5)).Return(nil)

	if rec := do(h, http.MethodDelete, "/api/sites/5", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

// TestDeleteSite_InvalidID: нечисловой, нулевой и отрицательный id → 400,
// и до сервиса такой запрос не доходит.
func TestDeleteSite_InvalidID(t *testing.T) {
	h, _ := newTestServer(t)

	for _, id := range []string{"abc", "0", "-1"} {
		if rec := do(h, http.MethodDelete, "/api/sites/"+id, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("id %q: status = %d, want %d", id, rec.Code, http.StatusBadRequest)
		}
	}
}

// TestListChecks_Limit: параметр ?limit= передаётся в сервис,
// а нечисловое значение отклоняется с кодом 400.
func TestListChecks_Limit(t *testing.T) {
	h, svc := newTestServer(t)
	svc.EXPECT().ListChecks(gomock.Any(), int64(3), 10).Return([]site.Check{{SiteID: 3, StatusCode: 200}}, nil)

	rec := do(h, http.MethodGet, "/api/sites/3/checks?limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := decodeBody[[]site.Check](t, rec); len(got) != 1 {
		t.Errorf("got %d checks, want 1", len(got))
	}

	// Переиспользуем rec (=, а не :=), чтобы не скрыть переменную выше.
	rec = do(h, http.MethodGet, "/api/sites/3/checks?limit=lots", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad limit: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// TestRunChecks_InProgress: если прогон уже идёт, клиент получает 409 Conflict.
func TestRunChecks_InProgress(t *testing.T) {
	h, svc := newTestServer(t)
	svc.EXPECT().RunChecks(gomock.Any()).Return(site.RunSummary{}, site.ErrCheckInProgress)

	if rec := do(h, http.MethodPost, "/api/checks/run", ""); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

// TestMethodNotAllowed: путь существует, но метод не тот — ServeMux сам
// отвечает 405 Method Not Allowed.
func TestMethodNotAllowed(t *testing.T) {
	h, _ := newTestServer(t)

	if rec := do(h, http.MethodPut, "/api/sites", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestRecoverer: паника внутри обработчика превращается в ответ 500,
// а сервер продолжает работать.
func TestRecoverer(t *testing.T) {
	h, svc := newTestServer(t)
	svc.EXPECT().ListSites(gomock.Any()).DoAndReturn(func(any) ([]site.Site, error) {
		panic("boom")
	})

	if rec := do(h, http.MethodGet, "/api/sites", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

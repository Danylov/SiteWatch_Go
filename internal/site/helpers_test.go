package site

// Вспомогательные функции для тестов пакета site.
// Файл оканчивается на _test.go, поэтому не попадает в рабочую сборку.

import (
	"io"
	"log/slog"
)

// discardLogger возвращает логгер, который всё выбрасывает (io.Discard),
// чтобы тесты не засоряли вывод сообщениями сервиса.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Пакет web встраивает файлы веб-интерфейса (HTML, JS, CSS) в исполняемый файл.
package web

import (
	"embed"
	"io/fs"
)

// files — встроенный каталог static целиком (с префиксом "static/" в путях).
//
//go:embed static
var files embed.FS

// Static возвращает файлы интерфейса без префикса "static/", чтобы
// index.html открывался по адресу "/", а не "/static/index.html".
func Static() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		// panic здесь оправдан: ошибка возможна только при опечатке в имени
		// каталога, а он проверяется компилятором при сборке. Это ошибка
		// программиста, а не ситуация, которую нужно обрабатывать в работе.
		panic(err)
	}
	return sub
}

package core

import (
	"html/template"
	"net/http"
)

type Page struct {
	url       string
	PageFiles []string
	Data      any
}

func RegisterPage(url string, pageInfo Page) {
	LoggerInstance.Info("Register page: url = %s, pageFiles = %#v", url, pageInfo.PageFiles)
	pageInfo.url = url
	pageMap[url] = pageInfo
}

func pageHandler(pageInfo Page) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse files html
		tmpl, err := template.ParseFiles((pageInfo.PageFiles[0]))
		if err != nil {
			panic(err)
		}
		// Execute template
		err = tmpl.Execute(w, pageInfo.Data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

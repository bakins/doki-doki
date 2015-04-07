package main

var defaultTemplate string = `
defaults
    timeout connect 500ms
    timeout client 5000ms
    timeout server 5000ms


frontend {{ .Name }}
{{ if eq .Protocol "http" }}
    mode http
{{ end }}
    bind *:{{ .Port }}
    default_backend be

backend be
{{ range $backend := .Backends }}
server {{ $backend.Name }} {{ $backend.IP }}:{{ $backend.Port }}
{{ end }}

`

// Package skills встраивает скиллы для агентов, чтобы сервис мог отдавать
// их по HTTP: агенту достаточно ссылки на /skill.md, чтобы узнать правила.
package skills

import _ "embed"

// CourtDebater — инструкция агенту-участнику дебатов (Agent Skills формат).
//
//go:embed court-debater/SKILL.md
var CourtDebater []byte

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// docMeta - клиентский JSON, разложенный по колонкам таблицы scans.
type docMeta struct {
	Type     string
	Num      *string    // docnum: без пробелов, тире и скобок
	Name     *string    // person_name
	Bday     *time.Time // person_bday (00:00 UTC)
	MetaInfo json.RawMessage
}

// docNumKeys - из каких полей "fields" собирается номер документа (значения склеиваются).
// Типы, которых здесь нет, номера не получают.
var docNumKeys = map[string][]string{
	"passport":       {"series", "number"},
	"passport_other": {"series", "number"},
	"snils":          {"snils"},
	"inn":            {"inn"},
	"driverlic":      {"number"},
	"driverlic_back": {"number"},
	"drivercard":     {"card_number"},
}

// skipMetaKeys - ключи верхнего уровня, которые в metainfo не сохраняются.
var skipMetaKeys = []string{"driver_kind", "source_file", "page", "date"}

// noPersonTypes - типы документов, для которых person_name и person_bday остаются пустыми.
var noPersonTypes = map[string]bool{"passport_other": true}

// splitMeta раскладывает проверенный JSON-объект клиента по колонкам.
//
// Из metainfo убираются skipMetaKeys, "type" и то, что ушло в колонки без потерь: фамилия/имя/отчество
// (только если они уже были в верхнем регистре и имя взято не из folder_person), birth_date и поля номера,
// если очистка (cleanDocNum) их не изменила (иначе, например, у "072-261-638 47" исходная запись пропала бы -
// такие поля остаются в metainfo). Значения, которые не удалось разобрать, тоже остаются на месте.
func splitMeta(m map[string]any) (*docMeta, error) {
	typ, _ := m["type"].(string)
	if typ = strings.TrimSpace(typ); typ == "" {
		return nil, clientErr(http.StatusUnprocessableEntity, `meta должен содержать строковое поле "type"`)
	}
	d := &docMeta{Type: typ}
	delete(m, "type")
	for _, k := range skipMetaKeys {
		delete(m, k)
	}

	fields, _ := m["fields"].(map[string]any)

	// номер документа
	if keys := docNumKeys[typ]; keys != nil && fields != nil {
		var num strings.Builder
		var used []string
		for _, k := range keys {
			s, ok := scalarString(fields[k])
			if !ok {
				continue
			}
			clean := cleanDocNum(s)
			num.WriteString(clean)
			if clean == s && clean != "" {
				used = append(used, k) // очистка ничего не убрала - из metainfo поле можно убрать без потерь
			}
		}
		if num.Len() > 0 {
			n := num.String()
			d.Num = &n
			for _, k := range used {
				delete(fields, k)
			}
		}
	}

	// у некоторых типов (страницы паспорта без данных владельца) имя и день рождения не заполняются
	if noPersonTypes[typ] {
		return d.encode(m)
	}

	// имя: "folder_person" (имя папки, из которой пришёл скан), а если его нет - фамилия имя отчество из
	// fields. Регистр верхний. folder_person из metainfo не убирается: от него остался бы только верхний регистр.
	if s, ok := m["folder_person"].(string); ok && strings.TrimSpace(s) != "" {
		n := strings.ToUpper(strings.Join(strings.Fields(s), " "))
		d.Name = &n
	} else {
		var parts, usedName []string
		for _, k := range []string{"surname", "name", "patronymic"} {
			if s, ok := fields[k].(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.ToUpper(strings.Join(strings.Fields(s), " ")))
				if s == strings.ToUpper(s) {
					usedName = append(usedName, k) // регистр не менялся - из fields можно убрать без потерь
				}
			}
		}
		if len(parts) > 0 {
			n := strings.Join(parts, " ")
			d.Name = &n
			for _, k := range usedName {
				delete(fields, k)
			}
		}
	}

	// день рождения
	if s, ok := fields["birth_date"].(string); ok {
		if t, ok := parseDate(s); ok {
			d.Bday = &t
			delete(fields, "birth_date")
		}
	}

	return d.encode(m)
}

// encode записывает остаток JSON клиента в MetaInfo.
func (d *docMeta) encode(m map[string]any) (*docMeta, error) {

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, clientErr(http.StatusUnprocessableEntity, "meta должен быть корректным JSON")
	}
	d.MetaInfo = bytes.TrimRight(buf.Bytes(), "\n")
	return d, nil
}

// scalarString - строка или число из JSON как текст.
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

// cleanDocNum убирает из номера документа пробелы (в том числе неразрывные), тире (дефис и типографские
// тире) и скобки. Буквы, регистр и остальные знаки сохраняются.
func cleanDocNum(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return -1
		case strings.ContainsRune("-‐‑‒–—−()[]{}", r):
			return -1
		}
		return r
	}, s)
}

// parseDate понимает "дд.мм.гггг" (как в JSON сканов) и "гггг-мм-дд".
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"02.01.2006", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

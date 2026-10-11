package api

import (
	"encoding/json"
	"testing"
)

func str(s string) *string { return &s }

func TestSplitMeta(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		doctype  string
		docnum   *string
		person   *string
		bday     string // "" - NULL
		metainfo string // ожидаемый metainfo (ключи в порядке encoding/json - по алфавиту)
	}{
		{
			name:    "паспорт: серия+номер, чистые значения уходят из metainfo",
			in:      `{"type":"passport","fields":{"series":"9218","number":"506640","surname":"ЕГОРОВ","name":"СЕРГЕЙ","patronymic":"АЛЕКСАНДРОВИЧ","birth_date":"09.11.1973","sex":"МУЖ"}}`,
			doctype: "passport", docnum: str("9218506640"), person: str("ЕГОРОВ СЕРГЕЙ АЛЕКСАНДРОВИЧ"), bday: "1973-11-09",
			metainfo: `{"fields":{"sex":"МУЖ"}}`,
		},
		{
			name:    "СНИЛС: имя из folder_person в верхнем регистре, fields и folder_person остаются в metainfo",
			in:      `{"type":"snils","folder_person":"Егоров Сергей","fields":{"snils":"072-261-638 47","surname":"Егоров","name":"Сергей"}}`,
			doctype: "snils", docnum: str("07226163847"), person: str("ЕГОРОВ СЕРГЕЙ"),
			metainfo: `{"fields":{"snils":"072-261-638 47","surname":"Егоров","name":"Сергей"},"folder_person":"Егоров Сергей"}`,
		},
		{
			name:    "пустой folder_person: имя из fields",
			in:      `{"type":"snils","folder_person":"  ","fields":{"snils":"07226163847","surname":"Егоров","name":"Сергей"}}`,
			doctype: "snils", docnum: str("07226163847"), person: str("ЕГОРОВ СЕРГЕЙ"),
			metainfo: `{"fields":{"surname":"Егоров","name":"Сергей"},"folder_person":"  "}`,
		},
		{
			name:    "ИНН: дату рождения не угадываем, dates остаются",
			in:      `{"type":"inn","fields":{"inn":"165016258750","dates":["09.11.1973","01.09.2000"]}}`,
			doctype: "inn", docnum: str("165016258750"),
			metainfo: `{"fields":{"dates":["09.11.1973","01.09.2000"]}}`,
		},
		{
			name:    "ИНН с birth_date",
			in:      `{"type":"inn","fields":{"inn":"165016258750","birth_date":"09.11.1973"}}`,
			doctype: "inn", docnum: str("165016258750"), bday: "1973-11-09",
			metainfo: `{"fields":{}}`,
		},
		{
			name:    "карта водителя: буквы в номере сохраняются, имя в верхнем регистре, исходный регистр в metainfo",
			in:      `{"type":"drivercard","fields":{"card_number":"RUD0000141616000","licence_number":"1607496663","surname":"Егоров","name":"Сергей","birth_date":"09.11.1973"}}`,
			doctype: "drivercard", docnum: str("RUD0000141616000"), person: str("ЕГОРОВ СЕРГЕЙ"), bday: "1973-11-09",
			metainfo: `{"fields":{"licence_number":"1607496663","surname":"Егоров","name":"Сергей"}}`,
		},
		{
			name:    "номер: убираются пробелы (и неразрывные), тире, скобки; буквы и регистр остаются",
			in:      `{"type":"driverlic","fields":{"number":"16 07-49(66)63– Ab"}}`,
			doctype: "driverlic", docnum: str("1607496663Ab"),
			metainfo: `{"fields":{"number":"16 07-49(66)63– Ab"}}`,
		},
		{
			name:    "номер из одних разделителей не пишется",
			in:      `{"type":"driverlic","fields":{"number":" - () "}}`,
			doctype: "driverlic", metainfo: `{"fields":{"number":" - () "}}`,
		},
		{
			name:    "passport_other: ни имени, ни дня рождения, ничего не убирается",
			in:      `{"type":"passport_other","folder_person":"Егоров Сергей","fields":{"series":"9218","number":"506640","surname":"ЕГОРОВ","birth_date":"09.11.1973","dates":["01.10.2008"]}}`,
			doctype: "passport_other", docnum: str("9218506640"),
			metainfo: `{"folder_person":"Егоров Сергей","fields":{"surname":"ЕГОРОВ","birth_date":"09.11.1973","dates":["01.10.2008"]}}`,
		},
		{
			name:    "ВУ: числовой номер из JSON-числа",
			in:      `{"type":"driverlic","fields":{"number":1607496663,"birth_date":"1973-11-09"}}`,
			doctype: "driverlic", docnum: str("1607496663"), bday: "1973-11-09",
			metainfo: `{"fields":{}}`,
		},
		{
			name:    "имя из папки, если в fields его нет; тип без номера",
			in:      `{"type":"passport_reg","folder_person":"  Фагизов   Халфат ","fields":{"address":"г. Москва"}}`,
			doctype: "passport_reg", person: str("ФАГИЗОВ ХАЛФАТ"),
			metainfo: `{"fields":{"address":"г. Москва"},"folder_person":"  Фагизов   Халфат "}`,
		},
		{
			name:     "непонятная дата остаётся в metainfo",
			in:       `{"type":"snils","fields":{"birth_date":"31.02.1973"}}`,
			doctype:  "snils",
			metainfo: `{"fields":{"birth_date":"31.02.1973"}}`,
		},
		{
			name:    "driver_kind, source_file, page и date в metainfo не попадают",
			in:      `{"type":"unknown","driver_kind":"skzi","source_file":"a.pdf","page":2,"date":"2022-04-01","image":"a.jpg","fields":{"date":"x","page":1}}`,
			doctype: "unknown", metainfo: `{"image":"a.jpg","fields":{"date":"x","page":1}}`,
		},
		{
			name:    "без fields",
			in:      `{"type":"unknown","image":"a.jpg"}`,
			doctype: "unknown", metainfo: `{"image":"a.jpg"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, err := parseMeta([]byte(c.in))
			if err != nil {
				t.Fatal(err)
			}
			d, err := splitMeta(obj)
			if err != nil {
				t.Fatal(err)
			}
			if d.Type != c.doctype {
				t.Errorf("doctype = %q, ожидался %q", d.Type, c.doctype)
			}
			eqPtr(t, "docnum", d.Num, c.docnum)
			eqPtr(t, "person_name", d.Name, c.person)
			var bday string
			if d.Bday != nil {
				bday = d.Bday.Format("2006-01-02")
			}
			if bday != c.bday {
				t.Errorf("person_bday = %q, ожидался %q", bday, c.bday)
			}
			var got, want any
			_ = json.Unmarshal(d.MetaInfo, &got)
			if err := json.Unmarshal([]byte(c.metainfo), &want); err != nil {
				t.Fatal(err)
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if string(gb) != string(wb) {
				t.Errorf("metainfo = %s, ожидалась %s", gb, wb)
			}
		})
	}
}

func eqPtr(t *testing.T, name string, got, want *string) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil || *got != *want:
		g, w := "NULL", "NULL"
		if got != nil {
			g = *got
		}
		if want != nil {
			w = *want
		}
		t.Errorf("%s = %s, ожидалось %s", name, g, w)
	}
}

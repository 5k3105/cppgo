// Package ir is the typed intermediate representation consumed by cppgo.
//
// Unlike miqt's string-typed model (where a parameter type is a string like
// "QList<Definition>" or "Flags" that must be re-parsed and re-qualified at every
// turn), every Type here is a STRUCTURED node whose identity is a USR. There is no
// spelling-based reasoning anywhere downstream — clang already resolved it.
package ir

import (
	"encoding/json"
	"os"
)

// Type is a structured, tagged-union type node emitted by the frontend.
type Type struct {
	Kind string `json:"kind"` // builtin|record|enum|pointer|ref|array|template|typedef|unexposed|unknown

	Name string `json:"name,omitempty"` // record/enum (qualified, display) | builtin
	USR  string `json:"usr,omitempty"`  // record/enum/template: canonical identity

	Const bool  `json:"const,omitempty"` // pointer/ref: is the pointee const
	Elem  *Type `json:"elem,omitempty"`  // pointer/ref/array element
	N     int64 `json:"n,omitempty"`     // array length (-1 = incomplete)

	Head string `json:"head,omitempty"` // template head, e.g. "QList", "QFlags"
	Args []Type `json:"args,omitempty"` // template arguments

	Alias     string `json:"alias,omitempty"`     // typedef: spelled alias
	Canonical *Type  `json:"canonical,omitempty"` // typedef: fully-resolved underlying

	Spelling string `json:"spelling,omitempty"` // unexposed fallback
}

type Param struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional"` // has a C++ default argument -> expanded into overloads
	Type     Type   `json:"type"`
}

type Method struct {
	Name       string  `json:"name"`
	Static     bool    `json:"static"`
	Const      bool    `json:"const"`
	Signal     bool    `json:"signal"`
	Virtual    bool    `json:"virtual"`
	Protected  bool    `json:"protected"`  // protected member -> exposed via the generated subclass
	Conversion bool    `json:"conversion"` // conversion operator (operator T()) -> bound as a To<T>() method
	Pure       bool    `json:"pure"`       // pure virtual (= 0): no base impl -> skip the <X>Default forwarder
	Own        bool    `json:"own"`        // declared in THIS class (not inherited-flattened) -> a non-virtual base-signature match is C++ name hiding, so gen re-emits it instead of de-flattening
	Ret        Type    `json:"ret"`
	Params     []Param `json:"params"`
}

type Base struct {
	USR    string `json:"usr"`
	Name   string `json:"name"`
	Access string `json:"access"`
}

type Record struct {
	USR          string   `json:"usr"`
	Name         string   `json:"name"`
	Abstract     bool     `json:"abstract"`    // has pure virtuals -> cannot be `new`ed
	DefaultCtor  bool     `json:"defaultCtor"` // default-constructible (gates Go-subclass machinery)
	IsQObject    bool     `json:"qobject"`     // is/derives QObject -> Qt-managed lifetime, no GC finalizer
	HasMetaObj   bool     `json:"metaObject"`  // declares Q_OBJECT/Q_GADGET (staticMetaObject) -> qobject_cast<T> legal
	Final        bool     `json:"final"`       // sealed class -> a generated <Cls>__Go subclass can't derive
	PublicDtor   bool     `json:"publicDtor"`  // destructor is public & usable -> `delete`/subclass is legal
	Copyable     bool     `json:"copyable"`    // copy ctor usable -> by-value bind (new Cls(other)) compiles
	Bases        []Base   `json:"bases"`
	Ctors        []Method `json:"ctors"`
	Methods      []Method `json:"methods"`
	PureVirtuals []Method `json:"pureVirtuals"`     // unimplemented pure virtuals (incl. inherited) to override
	Fields       []Field  `json:"fields"`           // PUBLIC data members -> synthesized Field()/SetField() accessors
	Header       string   `json:"header,omitempty"` // real defining header (frontend); the shim #includes exactly this
}

// Field is a PUBLIC data member, bound as a Go getter (FieldName()) + setter (SetFieldName(v)), like miqt.
type Field struct {
	Name  string `json:"name"`
	Const bool   `json:"const"` // const member -> getter only, no setter
	Type  Type   `json:"type"`
}

type Enum struct {
	USR    string      `json:"usr"`
	Name   string      `json:"name"`
	Header string      `json:"header,omitempty"` // real defining header (for per-owner enum file grouping)
	Scoped bool        `json:"scoped"`           // `enum class` -> qualify constants as Class__Enum__Const
	Public bool        `json:"public"`           // nameable by a free function (top-level or public-nested)
	Consts []EnumConst `json:"consts"`
}

// Func is a free (non-member) function: a package-level Go func, or — when it's a free/friend operator —
// a method attached to its first record argument.
type Func struct {
	Name    string  `json:"name"`    // C++ simple name ("operator==", "qFuzzyCompare")
	CppName string  `json:"cppName"` // fully-qualified spelling for the C++ call ("Qt::foo")
	Header  string  `json:"header,omitempty"`
	Ret     Type    `json:"ret"`
	Params  []Param `json:"params"`
}

type EnumConst struct {
	Name  string `json:"name"`
	Value string `json:"value"` // signed decimal string (preserves width/signedness)
}

type Typedef struct {
	USR       string `json:"usr"`
	Name      string `json:"name"`
	Canonical Type   `json:"canonical"`
}

type File struct {
	Header    string    `json:"header"`
	Records   []Record  `json:"records"`
	Enums     []Enum    `json:"enums"`
	Typedefs  []Typedef `json:"typedefs"`
	Functions []Func    `json:"functions"`
}

// Module is the merged IR of one or more frontend outputs, with a USR index.
type Module struct {
	Records   []Record
	Enums     []Enum
	Typedefs  []Typedef
	Functions []Func
	RecordUSR map[string]*Record
	EnumUSR   map[string]*Enum
}

func Load(paths ...string) (*Module, error) {
	m := &Module{RecordUSR: map[string]*Record{}, EnumUSR: map[string]*Enum{}}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var f File
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, err
		}
		for i := range f.Records {
			if f.Records[i].Header == "" { // frontend now supplies the real per-record header; fall back to the file's
				f.Records[i].Header = f.Header
			}
		}
		for i := range f.Functions {
			if f.Functions[i].Header == "" {
				f.Functions[i].Header = f.Header
			}
		}
		m.Records = append(m.Records, f.Records...)
		m.Enums = append(m.Enums, f.Enums...)
		m.Typedefs = append(m.Typedefs, f.Typedefs...)
		m.Functions = append(m.Functions, f.Functions...)
	}
	for i := range m.Records {
		if _, ok := m.RecordUSR[m.Records[i].USR]; !ok {
			m.RecordUSR[m.Records[i].USR] = &m.Records[i]
		}
	}
	for i := range m.Enums {
		m.EnumUSR[m.Enums[i].USR] = &m.Enums[i]
	}
	return m, nil
}

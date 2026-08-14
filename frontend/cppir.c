// cppir — the semantic extractor for the cppgo binding generator.
//
// Thesis: a C++->Go binding generator must not reconstruct meaning from clang's
// *spelled* type strings (that is what miqt does, and it is the root of its
// scope-minimal / typedef-chain / template / array bug class). Instead, let clang
// hand us the *resolved* meaning: every type is emitted as a STRUCTURED node whose
// identity is the canonical declaration's USR (Unified Symbol Resolution) — globally
// unique, scope-complete, version-stable. Typedefs carry their canonical resolution.
//
// Usage: cppir <header.h> <comma,sep,file,filters> [clang flags...]
// Emits a stable, USR-keyed JSON IR (our own schema) to stdout.
//
// Build: clang frontend/cppir.c -o cppir -lclang

#include <clang-c/Index.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// ---- file filter (only emit decls defined in headers matching one of these substrings) ----
static const char *g_filters[32];
static int g_nfilters = 0;

// ---- USR dedup set ----
static char *g_usrs[1 << 16];
static int g_nusr = 0;
static int seenUSR(CXCursor c) {
	CXString u = clang_getCursorUSR(c);
	const char *s = clang_getCString(u);
	int found = 0;
	if (s) {
		for (int i = 0; i < g_nusr; i++)
			if (strcmp(g_usrs[i], s) == 0) { found = 1; break; }
		if (!found && g_nusr < (1 << 16)) g_usrs[g_nusr++] = strdup(s);
	}
	clang_disposeString(u);
	return found;
}

// ---- a growable list of cursors ----
typedef struct { CXCursor *v; int n, cap; } CurList;
static void push(CurList *l, CXCursor c) {
	if (l->n >= l->cap) { l->cap = l->cap ? l->cap * 2 : 16; l->v = realloc(l->v, l->cap * sizeof(CXCursor)); }
	l->v[l->n++] = c;
}

static CurList g_recs = {0}, g_enums = {0}, g_tds = {0};

// ---- JSON string emit (escaped) ----
static void pstr(const char *s) {
	if (!s) return;
	for (; *s; s++) {
		unsigned char c = (unsigned char)*s;
		if (c == '"' || c == '\\') { putchar('\\'); putchar(c); }
		else if (c == '\n') fputs("\\n", stdout);
		else if (c < 0x20) printf("\\u%04x", c);
		else putchar(c);
	}
}
static void pcx(CXString s) { pstr(clang_getCString(s)); clang_disposeString(s); }

static const char *accessStr(enum CX_CXXAccessSpecifier a) {
	switch (a) {
	case CX_CXXPublic: return "public";
	case CX_CXXProtected: return "protected";
	case CX_CXXPrivate: return "private";
	default: return "none";
	}
}

static int fileMatches(CXCursor c) {
	CXSourceLocation loc = clang_getCursorLocation(c);
	CXFile f; clang_getSpellingLocation(loc, &f, 0, 0, 0);
	if (!f) return 0;
	CXString fn = clang_getFileName(f);
	const char *s = clang_getCString(fn);
	int m = (g_nfilters == 0);
	if (s && !m)
		for (int i = 0; i < g_nfilters; i++)
			if (strstr(s, g_filters[i])) { m = 1; break; }
	clang_disposeString(fn);
	return m;
}

// Fully-qualified name by walking semantic parents (display only; identity is the USR).
static const char *qualName(CXCursor c) {
	static char buf[4096];
	char parts[64][256]; int np = 0;
	for (CXCursor cur = c; !clang_Cursor_isNull(cur);) {
		enum CXCursorKind k = clang_getCursorKind(cur);
		if (k == CXCursor_TranslationUnit) break;
		if (k == CXCursor_Namespace || k == CXCursor_ClassDecl || k == CXCursor_StructDecl ||
		    k == CXCursor_ClassTemplate || k == CXCursor_ClassTemplatePartialSpecialization ||
		    k == CXCursor_EnumDecl) {
			CXString s = clang_getCursorSpelling(cur);
			const char *cs = clang_getCString(s);
			if (cs && cs[0] && np < 64) { strncpy(parts[np], cs, 255); parts[np][255] = 0; np++; }
			clang_disposeString(s);
		}
		cur = clang_getCursorSemanticParent(cur);
	}
	buf[0] = 0;
	for (int i = np - 1; i >= 0; i--) { strcat(buf, parts[i]); if (i > 0) strcat(buf, "::"); }
	return buf;
}

// ---- the heart: emit a STRUCTURED type, identity = canonical decl USR ----
static void emitType(CXType t) {
	switch (t.kind) {
	case CXType_Elaborated:
		emitType(clang_Type_getNamedType(t)); return; // unwrap "class Foo" elaboration
	case CXType_Pointer: {
		CXType p = clang_getPointeeType(t);
		printf("{\"kind\":\"pointer\",\"const\":%s,\"elem\":", clang_isConstQualifiedType(p) ? "true" : "false");
		emitType(p); printf("}"); return;
	}
	case CXType_LValueReference:
	case CXType_RValueReference: {
		CXType p = clang_getPointeeType(t);
		printf("{\"kind\":\"ref\",\"const\":%s,\"elem\":", clang_isConstQualifiedType(p) ? "true" : "false");
		emitType(p); printf("}"); return;
	}
	case CXType_ConstantArray:
		printf("{\"kind\":\"array\",\"n\":%lld,\"elem\":", (long long)clang_getArraySize(t));
		emitType(clang_getArrayElementType(t)); printf("}"); return;
	case CXType_IncompleteArray:
		printf("{\"kind\":\"array\",\"n\":-1,\"elem\":");
		emitType(clang_getArrayElementType(t)); printf("}"); return;
	case CXType_Typedef: {
		CXCursor d = clang_getTypeDeclaration(t);
		printf("{\"kind\":\"typedef\",\"alias\":\""); pcx(clang_getTypeSpelling(t));
		printf("\",\"usr\":\""); pcx(clang_getCursorUSR(d));
		printf("\",\"canonical\":"); emitType(clang_getCanonicalType(t)); // <-- the whole chain, resolved by clang
		printf("}"); return;
	}
	case CXType_Enum: {
		CXCursor d = clang_getTypeDeclaration(t);
		printf("{\"kind\":\"enum\",\"usr\":\""); pcx(clang_getCursorUSR(d));
		printf("\",\"name\":\""); pstr(qualName(d)); printf("\"}"); return;
	}
	case CXType_Record: {
		int n = clang_Type_getNumTemplateArguments(t);
		CXCursor d = clang_getTypeDeclaration(t);
		if (n > 0) { // class template specialization, e.g. QList<int>, QFlags<Flag>
			printf("{\"kind\":\"template\",\"head\":\""); pcx(clang_getCursorSpelling(d));
			printf("\",\"usr\":\""); pcx(clang_getCursorUSR(d));
			printf("\",\"args\":[");
			for (int i = 0; i < n; i++) {
				if (i) printf(",");
				CXType a = clang_Type_getTemplateArgumentAsType(t, i);
				if (a.kind == CXType_Invalid) printf("{\"kind\":\"unknown\"}");
				else emitType(a);
			}
			printf("]}"); return;
		}
		printf("{\"kind\":\"record\",\"usr\":\""); pcx(clang_getCursorUSR(d));
		printf("\",\"name\":\""); pstr(qualName(d)); printf("\"}"); return;
	}
	case CXType_Void: printf("{\"kind\":\"builtin\",\"name\":\"void\"}"); return;
	case CXType_Bool: printf("{\"kind\":\"builtin\",\"name\":\"bool\"}"); return;
	case CXType_Unexposed: {
		CXType can = clang_getCanonicalType(t);
		if (can.kind != CXType_Unexposed) { emitType(can); return; }
		printf("{\"kind\":\"unexposed\",\"spelling\":\""); pcx(clang_getTypeSpelling(t)); printf("\"}"); return;
	}
	default: { // int/char/float/double/long/... and anything else: a builtin by spelling
		CXString sp = clang_getTypeSpelling(t);
		const char *cs = clang_getCString(sp);
		printf("{\"kind\":\"builtin\",\"name\":\"");
		if (cs && strncmp(cs, "const ", 6) == 0) pstr(cs + 6); else pstr(cs);
		clang_disposeString(sp);
		printf("\"}"); return;
	}
	}
}

// ---- method / param emission ----
static enum CXChildVisitResult paramVisitor(CXCursor c, CXCursor p, CXClientData d) {
	(void)p;
	if (clang_getCursorKind(c) == CXCursor_ParmDecl) push((CurList *)d, c);
	return CXChildVisit_Continue;
}

static void emitMethod(CXCursor c, int isCtor) {
	printf("{\"name\":\""); pcx(clang_getCursorSpelling(c)); printf("\",");
	printf("\"static\":%s,", clang_CXXMethod_isStatic(c) ? "true" : "false");
	printf("\"const\":%s,", clang_CXXMethod_isConst(c) ? "true" : "false");
	if (!isCtor) { printf("\"ret\":"); emitType(clang_getCursorResultType(c)); printf(","); }
	printf("\"params\":[");
	CurList ps = {0};
	clang_visitChildren(c, paramVisitor, &ps);
	for (int i = 0; i < ps.n; i++) {
		if (i) printf(",");
		printf("{\"name\":\""); pcx(clang_getCursorSpelling(ps.v[i]));
		printf("\",\"type\":"); emitType(clang_getCursorType(ps.v[i])); printf("}");
	}
	free(ps.v);
	printf("]}");
}

// ---- record member collection ----
typedef struct { CurList bases, methods, ctors; } RecKids;
static enum CXChildVisitResult recKidVisitor(CXCursor c, CXCursor p, CXClientData d) {
	(void)p;
	RecKids *rk = (RecKids *)d;
	switch (clang_getCursorKind(c)) {
	case CXCursor_CXXBaseSpecifier: push(&rk->bases, c); break;
	case CXCursor_CXXMethod:
		if (clang_getCXXAccessSpecifier(c) == CX_CXXPublic) push(&rk->methods, c); break;
	case CXCursor_Constructor:
		if (clang_getCXXAccessSpecifier(c) == CX_CXXPublic) push(&rk->ctors, c); break;
	default: break;
	}
	return CXChildVisit_Continue; // don't recurse into members
}

static void emitRecord(CXCursor c) {
	RecKids rk = {0};
	clang_visitChildren(c, recKidVisitor, &rk);
	printf("{\"usr\":\""); pcx(clang_getCursorUSR(c));
	printf("\",\"name\":\""); pstr(qualName(c)); printf("\",");
	printf("\"bases\":[");
	for (int i = 0; i < rk.bases.n; i++) {
		if (i) printf(",");
		CXCursor b = rk.bases.v[i];
		CXCursor bd = clang_getTypeDeclaration(clang_getCursorType(b));
		printf("{\"usr\":\""); pcx(clang_getCursorUSR(bd));
		printf("\",\"name\":\""); pstr(qualName(bd));
		printf("\",\"access\":\"%s\"}", accessStr(clang_getCXXAccessSpecifier(b)));
	}
	printf("],\"ctors\":[");
	for (int i = 0; i < rk.ctors.n; i++) { if (i) printf(","); emitMethod(rk.ctors.v[i], 1); }
	printf("],\"methods\":[");
	for (int i = 0; i < rk.methods.n; i++) { if (i) printf(","); emitMethod(rk.methods.v[i], 0); }
	printf("]}");
	free(rk.bases.v); free(rk.methods.v); free(rk.ctors.v);
}

static void emitEnum(CXCursor c) {
	printf("{\"usr\":\""); pcx(clang_getCursorUSR(c));
	printf("\",\"name\":\""); pstr(qualName(c)); printf("\"}");
}

static void emitTypedef(CXCursor c) {
	printf("{\"usr\":\""); pcx(clang_getCursorUSR(c));
	printf("\",\"name\":\""); pstr(qualName(c));
	printf("\",\"canonical\":"); emitType(clang_getCanonicalType(clang_getTypedefDeclUnderlyingType(c)));
	printf("}");
}

// ---- top-level collection ----
static enum CXChildVisitResult topVisitor(CXCursor c, CXCursor p, CXClientData d) {
	(void)p; (void)d;
	enum CXCursorKind k = clang_getCursorKind(c);
	if ((k == CXCursor_ClassDecl || k == CXCursor_StructDecl) && clang_isCursorDefinition(c) && fileMatches(c)) {
		if (!seenUSR(c)) push(&g_recs, c);
	} else if (k == CXCursor_EnumDecl && clang_isCursorDefinition(c) && fileMatches(c)) {
		if (!seenUSR(c)) push(&g_enums, c);
	} else if ((k == CXCursor_TypedefDecl || k == CXCursor_TypeAliasDecl) && fileMatches(c)) {
		if (!seenUSR(c)) push(&g_tds, c);
	}
	return CXChildVisit_Recurse; // descend into namespaces and classes to reach nested decls
}

int main(int argc, char **argv) {
	if (argc < 3) { fprintf(stderr, "usage: cppir <header.h> <file,filters> [clang flags...]\n"); return 2; }
	const char *header = argv[1];
	char *fl = strdup(argv[2]);
	for (char *tok = strtok(fl, ","); tok && g_nfilters < 32; tok = strtok(0, ",")) g_filters[g_nfilters++] = tok;

	const char *args[512]; int na = 0;
	args[na++] = "-xc++-header"; args[na++] = "-std=c++17";
	// Pass clang's resource dir so its builtin headers (stddef.h -> size_t) are found; without it
	// the parse error-recovers size_t-dependent types to `int` (degraded std::string / QSpan<T>).
	static char rdbuf[1024];
	FILE *rp = popen("clang -print-resource-dir 2>/dev/null", "r");
	if (rp) {
		if (fgets(rdbuf, sizeof rdbuf, rp)) {
			size_t n = strlen(rdbuf);
			while (n && (rdbuf[n - 1] == '\n' || rdbuf[n - 1] == '\r')) rdbuf[--n] = 0;
			if (rdbuf[0]) { args[na++] = "-resource-dir"; args[na++] = rdbuf; }
		}
		pclose(rp);
	}
	for (int i = 3; i < argc && na < 510; i++) args[na++] = argv[i];

	CXIndex idx = clang_createIndex(0, 0);
	CXTranslationUnit tu = clang_parseTranslationUnit(
	    idx, header, args, na, 0, 0,
	    CXTranslationUnit_SkipFunctionBodies | CXTranslationUnit_KeepGoing);
	if (!tu) { fprintf(stderr, "cppir: failed to parse %s\n", header); return 1; }

	clang_visitChildren(clang_getTranslationUnitCursor(tu), topVisitor, 0);

	printf("{\"header\":\""); pstr(header); printf("\",\"records\":[");
	for (int i = 0; i < g_recs.n; i++) { if (i) printf(","); emitRecord(g_recs.v[i]); }
	printf("],\"enums\":[");
	for (int i = 0; i < g_enums.n; i++) { if (i) printf(","); emitEnum(g_enums.v[i]); }
	printf("],\"typedefs\":[");
	for (int i = 0; i < g_tds.n; i++) { if (i) printf(","); emitTypedef(g_tds.v[i]); }
	printf("]}\n");

	clang_disposeTranslationUnit(tu);
	clang_disposeIndex(idx);
	return 0;
}

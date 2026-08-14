// cppir2 — the LibTooling frontend for cppgo. Drop-in replacement for cppir.c: it emits
// the SAME USR-keyed JSON IR schema, but walks clang's full C++ AST (clang::QualType)
// instead of the libclang C API. That closes the C-API exposure gap that degraded
// QSpan<T> and std::string to `int` — here they resolve to proper template nodes, so the
// renderer drops them cleanly instead of silently producing wrong (int) bindings.
//
// Usage: cppir2 <header.h> <comma,sep,file,filters> [clang flags...]
// Build:  clang++ -std=c++17 -fno-rtti frontend/cppir2.cpp -o cppir2 -lclang-cpp -lLLVM

#include "clang/AST/ASTConsumer.h"
#include "clang/AST/ASTContext.h"
#include "clang/AST/CXXInheritance.h"
#include "clang/AST/DeclCXX.h"
#include "clang/AST/RecursiveASTVisitor.h"
#include "clang/AST/Type.h"
#include "clang/Basic/SourceManager.h"
#include "clang/Frontend/ASTUnit.h"
#include "clang/Frontend/CompilerInstance.h"
#include "clang/Frontend/FrontendAction.h"
#include "clang/Index/USRGeneration.h"
#include "clang/Tooling/Tooling.h"
#include "llvm/ADT/SmallString.h"

#include <cstdio>
#include <cstring>
#include <memory>
#include <set>
#include <string>
#include <vector>

using namespace clang;

static std::vector<std::string> g_filters;
static ASTContext *g_ctx = nullptr;
static std::string g_header; // the header arg, for the JSON "header" field (set in main)

// ---- JSON string emit (escaped) ----
static void pstr(const char *s) {
	if (!s)
		return;
	for (; *s; s++) {
		unsigned char c = (unsigned char)*s;
		if (c == '"' || c == '\\') { putchar('\\'); putchar(c); }
		else if (c == '\n') fputs("\\n", stdout);
		else if (c < 0x20) printf("\\u%04x", c);
		else putchar(c);
	}
}
static void pstr(const std::string &s) { pstr(s.c_str()); }

static std::string usrOf(const Decl *d) {
	llvm::SmallString<128> buf;
	if (!d || index::generateUSRForDecl(d, buf))
		return "";
	return std::string(buf.str());
}

static bool fileMatches(const Decl *d) {
	SourceManager &sm = g_ctx->getSourceManager();
	StringRef fn = sm.getFilename(sm.getSpellingLoc(d->getLocation()));
	// Reject Qt PRIVATE headers: *_p.h and *_impl.h begin with `#error Do not include ... directly`, so a
	// shim that #includes the decl's real defining header fails to compile (qfuture_impl.h,
	// qrangemodeladapter_impl.h). Their public API is reachable through the umbrella anyway.
	auto endsWith = [](StringRef s, const char *suf) {
		size_t n = std::strlen(suf);
		return s.size() >= n && s.substr(s.size() - n) == suf;
	};
	if (endsWith(fn, "_p.h") || endsWith(fn, "_impl.h"))
		return false;
	if (g_filters.empty())
		return true;
	for (auto &f : g_filters)
		if (fn.contains(f))
			return true;
	return false;
}

// headerOf is the decl's real defining header (its spelling-location file). The generated shim
// includes THIS, not the module umbrella, so each translation unit parses one small header instead
// of all of Qt -> compiles are fast and a fix touching one class rebuilds only that class.
static std::string headerOf(const Decl *d) {
	SourceManager &sm = g_ctx->getSourceManager();
	return sm.getFilename(sm.getSpellingLoc(d->getLocation())).str();
}

// ---- the heart: emit a STRUCTURED type with full AST fidelity ----
static void emitType(QualType qt) {
	// Preserve typedef sugar (alias + fully-resolved canonical), like the C frontend.
	if (const auto *tt = dyn_cast<TypedefType>(qt.getTypePtr())) {
		printf("{\"kind\":\"typedef\",\"alias\":\"");
		pstr(qt.getUnqualifiedType().getAsString());
		printf("\",\"usr\":\"");
		pstr(usrOf(tt->getDecl()));
		printf("\",\"canonical\":");
		emitType(qt.getCanonicalType()); // the whole chain, fully resolved by Sema
		printf("}");
		return;
	}

	const Type *t = qt.getTypePtr();

	if (const auto *pt = dyn_cast<PointerType>(t)) {
		QualType pe = pt->getPointeeType();
		printf("{\"kind\":\"pointer\",\"const\":%s,\"elem\":", pe.isConstQualified() ? "true" : "false");
		emitType(pe);
		printf("}");
		return;
	}
	if (const auto *rt = dyn_cast<ReferenceType>(t)) {
		QualType pe = rt->getPointeeType();
		// An rvalue reference (T&&) is a move parameter — we pass a borrowed lvalue handle, which can't
		// bind to it. Emit a distinct kind gen doesn't marshal, so the method is dropped (not mis-bound).
		const char *kind = isa<RValueReferenceType>(t) ? "rref" : "ref";
		printf("{\"kind\":\"%s\",\"const\":%s,\"elem\":", kind, pe.isConstQualified() ? "true" : "false");
		emitType(pe);
		printf("}");
		return;
	}
	if (const auto *at = g_ctx->getAsConstantArrayType(qt)) {
		printf("{\"kind\":\"array\",\"n\":%llu,\"elem\":", (unsigned long long)at->getSize().getZExtValue());
		emitType(at->getElementType());
		printf("}");
		return;
	}
	if (const auto *iat = dyn_cast<IncompleteArrayType>(t)) {
		printf("{\"kind\":\"array\",\"n\":-1,\"elem\":");
		emitType(iat->getElementType());
		printf("}");
		return;
	}
	if (const auto *et = dyn_cast<EnumType>(t)) {
		const EnumDecl *d = et->getDecl();
		printf("{\"kind\":\"enum\",\"usr\":\"");
		pstr(usrOf(d));
		printf("\",\"name\":\"");
		pstr(d->getQualifiedNameAsString());
		printf("\"}");
		return;
	}
	if (const auto *recT = dyn_cast<RecordType>(t)) {
		RecordDecl *rd = recT->getDecl();
		// Class template specialization: QList<X>, QFlags<Flag>, QSpan<char>, std::basic_string<...>
		if (auto *spec = dyn_cast<ClassTemplateSpecializationDecl>(rd)) {
			printf("{\"kind\":\"template\",\"head\":\"");
			pstr(spec->getSpecializedTemplate()->getNameAsString());
			printf("\",\"usr\":\"");
			pstr(usrOf(rd));
			printf("\",\"args\":[");
			const TemplateArgumentList &args = spec->getTemplateArgs();
			bool first = true;
			for (unsigned i = 0; i < args.size(); i++) {
				if (args[i].getKind() != TemplateArgument::Type)
					continue; // skip non-type args (e.g. QSpan's qsizetype Extent)
				if (!first)
					printf(",");
				first = false;
				emitType(args[i].getAsType());
			}
			printf("]}");
			return;
		}
		printf("{\"kind\":\"record\",\"usr\":\"");
		pstr(usrOf(rd));
		printf("\",\"name\":\"");
		pstr(rd->getQualifiedNameAsString());
		printf("\"}");
		return;
	}
	if (const auto *bt = dyn_cast<BuiltinType>(t)) {
		PrintingPolicy pp(g_ctx->getLangOpts());
		printf("{\"kind\":\"builtin\",\"name\":\"");
		pstr(bt->getName(pp).str());
		printf("\"}");
		return;
	}

	// Substituted template params / dependent / etc.: fall to canonical, else unexposed.
	QualType can = qt.getCanonicalType();
	if (can.getTypePtr() != t) {
		emitType(can);
		return;
	}
	printf("{\"kind\":\"unexposed\",\"spelling\":\"");
	pstr(qt.getAsString());
	printf("\"}");
}

static void emitMethod(const CXXMethodDecl *m, bool isCtor, bool isSignal = false, bool own = false) {
	printf("{\"name\":\"");
	pstr(m->getNameAsString());
	printf("\",\"static\":%s,\"const\":%s,\"signal\":%s,\"virtual\":%s,\"protected\":%s,\"conversion\":%s,\"pure\":%s,\"own\":%s,",
	       m->isStatic() ? "true" : "false", m->isConst() ? "true" : "false", isSignal ? "true" : "false",
	       (m->isVirtual() && !m->hasAttr<FinalAttr>()) ? "true" : "false",
	       m->getAccess() == AS_protected ? "true" : "false",
	       isa<CXXConversionDecl>(m) ? "true" : "false",
	       // pure: a pure virtual (= 0) has NO out-of-line definition, so a `<X>Default` base-call
	       // forwarder (Base::method()) would be an undefined reference at link. gen skips Default when pure.
	       m->isPureVirtual() ? "true" : "false",
	       // own: declared in the record being emitted (not an inherited-flattened copy). gen uses it to
	       // tell C++ NAME HIDING (a derived non-virtual re-declaration with a base-matching signature,
	       // which must be re-emitted so the derived impl is called) apart from a plain inherited method
	       // (which de-flattens onto Go embedding).
	       own ? "true" : "false");
	if (!isCtor) {
		printf("\"ret\":");
		emitType(m->getReturnType());
		printf(",");
	}
	printf("\"params\":[");
	bool first = true;
	for (const ParmVarDecl *p : m->parameters()) {
		if (!first)
			printf(",");
		first = false;
		printf("{\"name\":\"");
		pstr(p->getNameAsString());
		printf("\",\"optional\":%s,\"type\":", p->hasDefaultArg() ? "true" : "false");
		emitType(p->getType());
		printf("}");
	}
	printf("]}");
}

// emitFunction emits a free function: name (for operator detection / Go naming), cppName (the fully-
// qualified spelling used in the C++ call, e.g. "qFuzzyCompare" or "Qt::foo"), header, ret, and params.
static void emitFunction(const FunctionDecl *d) {
	printf("{\"name\":\"");
	pstr(d->getNameAsString());
	printf("\",\"cppName\":\"");
	pstr(d->getQualifiedNameAsString());
	printf("\",\"header\":\"");
	pstr(headerOf(d));
	printf("\",\"ret\":");
	emitType(d->getReturnType());
	printf(",\"params\":[");
	bool first = true;
	for (const ParmVarDecl *p : d->parameters()) {
		if (!first)
			printf(",");
		first = false;
		printf("{\"name\":\"");
		pstr(p->getNameAsString());
		printf("\",\"optional\":%s,\"type\":", p->hasDefaultArg() ? "true" : "false");
		emitType(p->getType());
		printf("}");
	}
	printf("]}");
}

// inheritsQObject reports whether d is QObject or transitively derives from it. QObject lifetime is
// managed by Qt (parent/child, deleteLater), so such wrappers must not get a Go GC finalizer.
static bool inheritsQObject(const CXXRecordDecl *d) {
	if (!d || !d->hasDefinition())
		return d && d->getQualifiedNameAsString() == "QObject";
	if (d->getQualifiedNameAsString() == "QObject")
		return true;
	for (const auto &b : d->bases())
		if (inheritsQObject(b.getType()->getAsCXXRecordDecl()))
			return true;
	return false;
}

// collectMethods gathers d's public methods AND those inherited from all bases, deduped by
// name+signature (the most-derived override wins, since d is walked first). Without this, a bound
// class can't call any base-class API — and an overridable inherited virtual (rowCount/data on a
// QAbstractListModel subclass) couldn't be invoked.
static void collectMethods(const CXXRecordDecl *rd, std::set<std::string> &seen,
                           std::set<std::string> &shadowed, std::vector<const CXXMethodDecl *> &out) {
	if (!rd || !rd->hasDefinition())
		return;
	// C++ name hiding: declaring ANY method `foo` in a derived class hides EVERY base overload of
	// `foo` (absent a using-declaration), so those base overloads aren't callable via the derived
	// pointer. `shadowed` holds names declared by a more-derived class; `mine` the names declared
	// here. Collect `mine` first so all of this class's own overloads survive even as we hide bases.
	std::set<std::string> mine;
	for (const CXXMethodDecl *m : rd->methods())
		if (!isa<CXXConstructorDecl>(m) && !isa<CXXDestructorDecl>(m))
			mine.insert(m->getNameAsString());
	// A private/protected using-declaration (e.g. KProcess: `private: using QProcess::
	// setProcessChannelMode;`) makes an inherited name inaccessible via the derived pointer. Treat it
	// like a redeclaration: shadow that name so the base's public version isn't collected as callable.
	for (const Decl *sub : rd->decls())
		if (const auto *us = dyn_cast<UsingShadowDecl>(sub))
			if (us->getAccess() == AS_private || us->getAccess() == AS_protected)
				mine.insert(us->getDeclName().getAsString());
	for (const CXXMethodDecl *m : rd->methods()) {
		// Overloaded OPERATORS are KEPT (gen binds the valuable subset — comparison/arithmetic/bitwise/
		// subscript — with miqt-style names like OperatorEqual/OperatorPlus, and skips the rest). CONVERSION
		// operators are KEPT too (tagged "conversion":true; gen binds them as To<Type>() methods, or drops
		// the ones whose target isn't bindable). Only ctors/dtors are dropped here.
		if (isa<CXXConstructorDecl>(m) || isa<CXXDestructorDecl>(m))
			continue;
		std::string name = m->getNameAsString();
		// Qt meta-object helpers (Q_OBJECT/Q_GADGET expansions) are not real API: qt_check_for_QGADGET_macro
		// is DECLARED-BUT-NEVER-DEFINED (a compile-time macro guard) so binding it → undefined reference at
		// link; qt_metacall/qt_metacast/qt_static_metacall are moc-internal. Skip the whole qt_ family.
		if (name.rfind("qt_", 0) == 0)
			continue;
		// KDE binary-compat extensibility hooks: `virtual void virtual_hook(int, void*)` is declared in
		// several UNRELATED bases (KXMLGUIClient + KXMLGUIBuilder, KConfigBase, ...), so a class inheriting
		// two of them (KXmlGuiWindow, KParts::MainWindow) makes the name ambiguous -> the generated override
		// `Cls::virtual_hook(...)` won't compile. It carries no projectable API; skip like the qt_ helpers.
		if (name == "virtual_hook")
			continue;
		// A =delete'd method can't be called — binding a shim that invokes it is a hard C++ error (e.g.
		// KPluginMetaData::value(const QString&, const char*) is deleted to force a typed overload). Ctors
		// already skip isDeleted() above; methods need the same guard.
		if (m->isDeleted())
			continue;
		std::string key = name + "|" + m->getType().getAsString();
		if (seen.count(key))
			continue;
		seen.insert(key); // mark seen even when non-public, so a base's public copy can't re-add a
		                  // method a derived class deliberately hid (e.g. QAbstractListModel makes
		                  // columnCount/parent private) -> calling it via the derived ptr is illegal.
		if (shadowed.count(name)) // a more-derived class redeclared this name -> base overload hidden
			continue;
		// Include PROTECTED methods (tagged via emitMethod's access check) — gen exposes a non-virtual
		// protected method through the generated subclass (e.g. QAbstractItemModel::createIndex). Skip
		// only private.
		if (m->getAccess() == AS_private)
			continue;
		out.push_back(m);
	}
	std::set<std::string> deeper = shadowed;
	deeper.insert(mine.begin(), mine.end());
	for (const auto &b : rd->bases())
		collectMethods(b.getType()->getAsCXXRecordDecl(), seen, deeper, out);
}

// isSignalMethod reports whether m is declared under a `Q_SIGNALS:` (qt_signal-annotated) access
// specifier in ITS OWN declaring class — so it works for inherited signals too (a derived class that
// inherits a signal must still not call it as a plain method, e.g. KCodecAction <- KSelectAction::
// actionTriggered). Qt's `signals` expands to `public`, so access alone can't distinguish them.
static bool isSignalMethod(const CXXMethodDecl *m) {
	const CXXRecordDecl *rd = m ? m->getParent() : nullptr;
	if (!rd)
		return false;
	bool inSignals = false;
	for (const Decl *sub : rd->decls()) {
		if (const auto *as = dyn_cast<AccessSpecDecl>(sub)) {
			inSignals = false;
			for (const auto *attr : as->attrs())
				if (const auto *ann = dyn_cast<AnnotateAttr>(attr))
					if (ann->getAnnotation() == "qt_signal")
						inSignals = true;
		} else if (sub == m) {
			return inSignals;
		}
	}
	return false;
}

// hasQObjectMacro reports whether d itself declares the Q_OBJECT/Q_GADGET machinery (a `staticMetaObject`
// static member). qobject_cast<T*> static_asserts unless T has it — so a class that inheritsQObject but
// LACKS the macro (e.g. KViewStateMaintainerBase) must NOT get a generated Cast<T>. Only the declaring
// class is checked (not bases): qobject_cast keys off the target type's own metaobject.
static bool hasQObjectMacro(const CXXRecordDecl *d) {
	if (!d || !d->hasDefinition())
		return false;
	for (const Decl *sub : d->decls())
		if (const auto *vd = dyn_cast<VarDecl>(sub))
			if (vd->isStaticDataMember() && vd->getName() == "staticMetaObject")
				return true;
	return false;
}

static void emitRecord(const CXXRecordDecl *d) {
	printf("{\"usr\":\"");
	pstr(usrOf(d));
	printf("\",\"name\":\"");
	pstr(d->getQualifiedNameAsString());
	printf("\",\"header\":\"");
	pstr(headerOf(d));
	// publicDtor: can code outside the class `delete` an instance / derive-and-destruct? False for a
	// protected/private/deleted destructor (e.g. QStandardPaths, QIODeviceBase) -> no delete shim, no
	// Go-subclass. final: the class is sealed, so a generated <Cls>__Go subclass won't compile.
	const CXXDestructorDecl *dd = d->getDestructor();
	bool publicDtor = !dd || (dd->getAccess() == AS_public && !dd->isDeleted());
	// copyable: can an instance be copied into our owned-heap model (new Cls(other))? False for
	// move-only types (Q_DISABLE_COPY -> deleted copy ctor, e.g. QStringConverter). A by-value bind of
	// such a type would emit a copy that won't compile, so gen drops those methods instead.
	// defaultedCopyConstructorIsDeleted() asserts unless Sema has computed the copy-ctor property
	// (true when no overload resolution is needed). Guard it; a user-declared copy ctor (handled in the
	// loop below) is the authoritative signal anyway. Default to copyable when we can't safely ask.
	bool copyable = true;
	if (!d->needsOverloadResolutionForCopyConstructor())
		copyable = !d->defaultedCopyConstructorIsDeleted();
	// defaultCtor must be PUBLIC to be usable: hasDefaultConstructor() is true even for a private one
	// (QStyleHints, QOpenGLContextGroup — Qt constructs them internally), but the generated subclass's
	// `: Cls()` then can't call it. Start from hasDefaultConstructor (covers the implicit-public case
	// where no ctor is user-declared) and, if one IS declared, take its actual accessibility.
	bool defaultCtor = d->hasDefaultConstructor();
	for (const CXXMethodDecl *m : d->methods())
		if (const auto *c = dyn_cast<CXXConstructorDecl>(m)) {
			if (c->isCopyConstructor())
				copyable = !c->isDeleted() && c->getAccess() == AS_public;
			if (c->isDefaultConstructor())
				defaultCtor = c->getAccess() == AS_public && !c->isDeleted();
		}
	printf("\",\"abstract\":%s,\"defaultCtor\":%s,\"qobject\":%s,\"metaObject\":%s,\"final\":%s,\"publicDtor\":%s,\"copyable\":%s,\"bases\":[",
	       d->isAbstract() ? "true" : "false", defaultCtor ? "true" : "false",
	       inheritsQObject(d) ? "true" : "false", hasQObjectMacro(d) ? "true" : "false",
	       d->isEffectivelyFinal() ? "true" : "false",
	       publicDtor ? "true" : "false", copyable ? "true" : "false");
	bool first = true;
	for (const auto &b : d->bases()) {
		const CXXRecordDecl *bd = b.getType()->getAsCXXRecordDecl();
		if (!bd)
			continue;
		if (!first)
			printf(",");
		first = false;
		const char *acc = b.getAccessSpecifier() == AS_public ? "public" : (b.getAccessSpecifier() == AS_protected ? "protected" : "private");
		printf("{\"usr\":\"");
		pstr(usrOf(bd));
		printf("\",\"name\":\"");
		pstr(bd->getQualifiedNameAsString());
		printf("\",\"access\":\"%s\"}", acc);
	}
	printf("],\"ctors\":[");
	first = true;
	for (const CXXMethodDecl *m : d->methods()) {
		if (!isa<CXXConstructorDecl>(m) || m->getAccess() != AS_public)
			continue;
		const auto *ctor = cast<CXXConstructorDecl>(m);
		// COPY constructors ARE bound — miqt binds them too (NewQVariant3(*QVariant) = the copy ctor), and
		// keeping them keeps cppgo's NewClass<N> numbering IN SYNC with miqt, removing the per-class ctor-
		// remap step when porting a miqt app. A deleted copy ctor (move-only types: Q_DISABLE_COPY) is caught
		// by isDeleted() below. MOVE constructors are still skipped: the rvalue-ref param has no stable Go
		// projection (and miqt doesn't bind them either). DELETED ctors can't be called (won't compile).
		if (ctor->isMoveConstructor() || ctor->isDeleted())
			continue;
		if (!first)
			printf(",");
		first = false;
		emitMethod(m, true);
	}
	printf("],\"methods\":[");
	// Signals (declared under `Q_SIGNALS:`) are detected per-method via isSignalMethod, so an INHERITED
	// signal is tagged too and routed to the signal set instead of leaking as a callable method.
	std::set<std::string> mseen, mshadowed;
	std::vector<const CXXMethodDecl *> allm;
	collectMethods(d, mseen, mshadowed, allm);
	first = true;
	for (const CXXMethodDecl *m : allm) {
		if (!first)
			printf(",");
		first = false;
		emitMethod(m, false, isSignalMethod(m), m->getParent() == d);
	}
	// Pure virtuals (INCLUDING inherited ones) still unimplemented in d -- a concrete Go subclass must
	// override every one of them. Use the final-overrider map: a virtual is still pure iff its final
	// overrider is pure. (data()/rowCount() of QAbstractListModel are inherited, not re-declared, so
	// d->methods() alone would miss them.)
	printf("],\"pureVirtuals\":[");
	if (d->isAbstract()) {
		CXXFinalOverriderMap fom;
		d->getFinalOverriders(fom);
		std::set<std::string> seenPV;
		bool firstPV = true;
		for (const auto &kv : fom)
			for (const auto &so : kv.second)
				for (const auto &um : so.second) {
					const CXXMethodDecl *fm = um.Method;
					if (!fm->isPureVirtual() || isa<CXXDestructorDecl>(fm))
						continue;
					std::string u = usrOf(fm);
					if (seenPV.count(u))
						continue;
					seenPV.insert(u);
					if (!firstPV)
						printf(",");
					firstPV = false;
					emitMethod(fm, false, false);
				}
	}
	// PUBLIC data members -> gen synthesizes a Field() getter and SetField() setter (like miqt), giving
	// access to struct fields cppgo's method-only binding otherwise omits (QStyleOption flags/rect, QImage
	// fields, QAccessible::State bitfields, ...). Own (declared-here) fields only; derived classes inherit
	// the getters via Go embedding, mirroring method de-flattening. Non-static by construction (d->fields()).
	printf("],\"fields\":[");
	{
		bool firstF = true;
		for (const FieldDecl *fd : d->fields()) {
			if (fd->getAccess() != AS_public || !fd->getDeclName() || fd->getNameAsString().empty())
				continue;
			if (!firstF)
				printf(",");
			firstF = false;
			printf("{\"name\":\"");
			pstr(fd->getNameAsString());
			printf("\",\"const\":%s,\"type\":", fd->getType().isConstQualified() ? "true" : "false");
			emitType(fd->getType());
			printf("}");
		}
	}
	printf("]}");
}

// ---- collection ----
struct Collector : RecursiveASTVisitor<Collector> {
	std::set<std::string> seen;
	std::vector<const CXXRecordDecl *> recs;
	std::vector<const EnumDecl *> enums;
	std::vector<const TypedefNameDecl *> tds;
	std::vector<const FunctionDecl *> funcs; // free (non-member) functions incl. free/friend operators

	bool fresh(const Decl *d) {
		std::string u = usrOf(d);
		if (u.empty() || seen.count(u))
			return false;
		seen.insert(u);
		return true;
	}
	bool VisitCXXRecordDecl(const CXXRecordDecl *d) {
		if (!(d->isThisDeclarationADefinition() && fileMatches(d) && !d->isImplicit() && d->getDeclName()))
			return true;
		// Whole-module structural filters: skip records that are not bindable public classes. (a) class
		// templates — the generic `template<class T> struct X` has no concrete type. (b) template
		// specializations — containers (QList<int>) are handled by type-marshalling. (c) anonymous-
		// namespace / function-local classes — TU-internal helpers (e.g. `Destructor`).
		if (d->getDescribedClassTemplate() || isa<ClassTemplateSpecializationDecl>(d) ||
		    d->isInAnonymousNamespace() || d->isLocalClass())
			return true;
		// IRREDUCIBLE LINK-ONLY RESIDUAL: classes that appear in a public header but have NO linkable symbol
		// in the Qt library (binding them compiles, then fails at executable link with "undefined reference").
		// Header inspection alone can't detect this; the list is found by ONE link pass and kept SMALL (it is
		// miqt's curated AllowClass knowledge). QtPrivate:: = Qt's internal namespace; QBrushDataPointerDeleter
		// = a header-only deleter functor (its operator() is never compiled into the lib).
		// NOTE (Bug-3 / unexported-symbol residual): an export-VISIBILITY heuristic was tried here —
		// skip a class with a user-written OUT-OF-LINE ctor but no `hasAttr<VisibilityAttr>()` — and REJECTED.
		// It over-skipped 57 legitimate classes (QModelIndex, QPolygon, QStringConverter, QSignalBlocker, ...):
		// `hasAttr<VisibilityAttr>` does NOT reliably mean "exported" (Q_CORE_EXPORT on QModelIndex did not
		// register), and out-of-line user ctors are common in perfectly linkable classes. The info needed
		// ("is this symbol in the .so?") is NOT reliably in the header — it's set at the library's compile
		// time. So this residual is detected at the LINK (tools/check_link_smoke.sh) and handled by the small
		// curated list in cmd/gen (KAboutRelease, ...). Detection happens at the earliest stage that actually
		// has the information — which here is the link, not the frontend.
		{
			std::string qn = d->getQualifiedNameAsString();
			if (qn.rfind("QtPrivate::", 0) == 0 || qn == "QBrushDataPointerDeleter")
				return true;
		}
		// PUBLIC nested records ARE bound (flattened: QTextEdit::ExtraSelection -> QTextEdit__ExtraSelection,
		// QTextLayout::FormatRange, ...) — gen qualifies them. But skip: (a) a non-public nested record
		// (private impl detail); (b) a record nested inside a TEMPLATE class (QHash<K,V>::iterator,
		// QList::const_iterator, ...) — a dependent type that can't be named/bound standalone.
		if (const auto *dc = dyn_cast<CXXRecordDecl>(d->getDeclContext())) {
			if (dc->getDescribedClassTemplate() || isa<ClassTemplateSpecializationDecl>(dc))
				return true;
			if (dc->hasDefinition() && d->getAccess() != AS_public)
				return true;
			// Nested iterators (QCborArray::Iterator, ...) reference the container's dependent value_type
			// and can't be bound standalone — skip by name.
			std::string nm = d->getNameAsString();
			if (nm.find("iterator") != std::string::npos || nm.find("Iterator") != std::string::npos)
				return true;
		}
		if (fresh(d))
			recs.push_back(d);
		return true;
	}
	bool VisitEnumDecl(const EnumDecl *d) {
		if (d->isThisDeclarationADefinition() && fileMatches(d) && d->getDeclName() && fresh(d))
			enums.push_back(d);
		return true;
	}
	bool VisitTypedefNameDecl(const TypedefNameDecl *d) {
		if (fileMatches(d) && d->getDeclName() && fresh(d))
			tds.push_back(d);
		return true;
	}
	// Free (non-member) functions: qFuzzyCompare / qChecksum / ... become package-level Go funcs, and
	// free/friend operators (operator==(QPoint,QPoint), ...) get attached to their first record argument
	// as a method. Members are handled via records; templates have no concrete type; static/anonymous =
	// internal linkage; deduction guides aren't callable.
	bool VisitFunctionDecl(const FunctionDecl *d) {
		if (isa<CXXMethodDecl>(d) || isa<CXXDeductionGuideDecl>(d))
			return true;
		if (!fileMatches(d) || d->isImplicit() || !d->getDeclName())
			return true;
		if (d->getDescribedFunctionTemplate() || d->isFunctionTemplateSpecialization())
			return true;
		if (d->isDeleted() || d->isInAnonymousNamespace() || d->getStorageClass() == SC_Static)
			return true;
		// Qt INTERNAL namespace: QtPrivate::indexOf/contains/... are implementation helpers with no public,
		// linkable symbol — binding them compiles but is an undefined reference at executable link. (miqt
		// rejects QtPrivate:: in AllowType.) This is part of the small irreducible link-only residual.
		if (d->getQualifiedNameAsString().rfind("QtPrivate::", 0) == 0)
			return true;
		// Hidden friends — a `friend` function DEFINED in a class body, found only by ADL (e.g.
		// Qt::partial_ordering's is_eq/is_neq/is_lt/...) — are NOT callable by qualified name, but gen emits
		// the qualified cppName (`Qt::is_eq(...)`) -> "is_eq is not a member of Qt". Friend OPERATORS are
		// still wanted (gen calls them with operator syntax / by operand ADL), so skip only non-operators.
		if (d->getFriendObjectKind() != Decl::FOK_None && !d->isOverloadedOperator())
			return true;
		if (fresh(d))
			funcs.push_back(d);
		return true;
	}
};

// emitTU walks the parsed translation unit and prints the USR-keyed JSON IR to stdout. It runs as the
// ASTConsumer of CppirAction below (so the parse can be configured — see SkipFunctionBodies).
static void emitTU(ASTContext &ctx) {
	g_ctx = &ctx;
	Collector c;
	c.TraverseDecl(ctx.getTranslationUnitDecl());

	printf("{\"header\":\"");
	pstr(g_header.c_str());
	printf("\",\"records\":[");
	for (size_t i = 0; i < c.recs.size(); i++) {
		if (i)
			printf(",");
		emitRecord(c.recs[i]);
	}
	printf("],\"enums\":[");
	for (size_t i = 0; i < c.enums.size(); i++) {
		if (i)
			printf(",");
		const EnumDecl *d = c.enums[i];
		printf("{\"usr\":\"");
		pstr(usrOf(d));
		printf("\",\"name\":\"");
		pstr(d->getQualifiedNameAsString());
		printf("\",\"header\":\"");
		pstr(headerOf(d));
		// scoped: `enum class` (or `enum struct`). gen qualifies a scoped enum's constants with the enum
		// name (Class__Enum__Const) to avoid colliding a scoped enumerator with an unscoped enum/type of
		// the same flattened name (e.g. QPdfDocument::Status::Error vs the QPdfDocument::Error enum type).
		// public: can a FREE function name this enum type? True for a top-level enum (access AS_none) or a
		// PUBLIC nested enum; false for a protected/private nested enum (QGraphicsItem::Extension) — gen
		// then skips Defaults whose signature names an INACCESSIBLE enum (a free <X>Default shim can't spell it).
		printf("\",\"scoped\":%s,\"public\":%s,\"consts\":[", d->isScoped() ? "true" : "false",
		       (d->getAccess() == AS_public || d->getAccess() == AS_none) ? "true" : "false");
		bool firstC = true;
		for (const EnumConstantDecl *ec : d->enumerators()) {
			if (!firstC)
				printf(",");
			firstC = false;
			printf("{\"name\":\"");
			pstr(ec->getNameAsString());
			llvm::SmallString<32> sv;
			ec->getInitVal().toString(sv);
			printf("\",\"value\":\"");
			pstr(std::string(sv.c_str()));
			printf("\"}");
		}
		printf("]}");
	}
	printf("],\"typedefs\":[");
	for (size_t i = 0; i < c.tds.size(); i++) {
		if (i)
			printf(",");
		const TypedefNameDecl *d = c.tds[i];
		printf("{\"usr\":\"");
		pstr(usrOf(d));
		printf("\",\"name\":\"");
		pstr(d->getQualifiedNameAsString());
		printf("\",\"canonical\":");
		emitType(d->getUnderlyingType().getCanonicalType());
		printf("}");
	}
	printf("],\"functions\":[");
	for (size_t i = 0; i < c.funcs.size(); i++) {
		if (i)
			printf(",");
		emitFunction(c.funcs[i]);
	}
	printf("]}\n");
}

struct CppirConsumer : ASTConsumer {
	void HandleTranslationUnit(ASTContext &ctx) override { emitTU(ctx); }
};

struct CppirAction : ASTFrontendAction {
	// cppir2 reads only DECLARATIONS — class/base/method signatures, types, USRs, enums, default-args —
	// never function bodies. Skip parsing/Sema of every inline & template body across the whole Qt/KF6
	// surface (exactly what clangd/indexers do for speed): a large cut to the cold-parse time AND the
	// peak AST memory. Declarations/types/USRs are unaffected; graceful if the flag has no effect.
	bool BeginInvocation(CompilerInstance &CI) override {
		CI.getFrontendOpts().SkipFunctionBodies = true;
		return true;
	}
	std::unique_ptr<ASTConsumer> CreateASTConsumer(CompilerInstance &, StringRef) override {
		return std::make_unique<CppirConsumer>();
	}
};

int main(int argc, char **argv) {
	if (argc < 3) {
		fprintf(stderr, "usage: cppir2 <header.h> <file,filters> [clang flags...]\n");
		return 2;
	}
	std::string header = argv[1];
	std::string flt = argv[2];
	for (size_t i = 0, j; i <= flt.size(); i = j + 1) {
		j = flt.find(',', i);
		if (j == std::string::npos)
			j = flt.size();
		if (j > i)
			g_filters.push_back(flt.substr(i, j - i));
	}

	// QT_ANNOTATE_ACCESS_SPECIFIER makes Qt tag `signals:`/`slots:` access specifiers with an
	// annotate attribute we can read (see emitRecord); without it signals are indistinguishable
	// from ordinary public methods.
	std::vector<std::string> args = {"-std=c++17",
	                                 "-DQT_ANNOTATE_ACCESS_SPECIFIER(x)=__attribute__((annotate(#x)))"};
	// Clang's builtin headers (stddef.h, etc.) live in its resource dir; a standalone tool can't
	// locate it relative to its own binary, so we detect and pass it. WITHOUT this, parsing
	// anything that transitively needs size_t fails and clang error-recovers by substituting
	// `int` — which silently degraded std::string and QSpan<T> (this bit both frontends).
	if (FILE *p = popen("clang -print-resource-dir 2>/dev/null", "r")) {
		char buf[1024];
		if (fgets(buf, sizeof buf, p)) {
			std::string rd(buf);
			while (!rd.empty() && (rd.back() == '\n' || rd.back() == '\r'))
				rd.pop_back();
			if (!rd.empty()) {
				args.push_back("-resource-dir");
				args.push_back(rd);
			}
		}
		pclose(p);
	}
	for (int i = 3; i < argc; i++)
		args.push_back(argv[i]);

	std::string code = "#include \"" + header + "\"\n";
	g_header = header;
	// Parse via CppirAction so the invocation can SkipFunctionBodies; its consumer (emitTU) prints the IR.
	if (!tooling::runToolOnCodeWithArgs(std::make_unique<CppirAction>(), code, args, "input.cc", "cppir2")) {
		fprintf(stderr, "cppir2: failed to build AST for %s\n", header.c_str());
		return 1;
	}
	return 0;
}

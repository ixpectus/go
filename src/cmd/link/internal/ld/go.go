// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// go-specific code shared across loaders (5l, 6l, 8l).

package ld

import (
	"bytes"
	"cmd/internal/bio"
	"cmd/internal/objabi"
	"cmd/link/internal/loader"
	"cmd/link/internal/sym"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// go-specific code shared across loaders (5l, 6l, 8l).

// replace all "". with pkg.
func expandpkg(t0 string, pkg string) string {
	return strings.Replace(t0, `"".`, pkg+".", -1)
}

func resolveABIAlias(s *sym.Symbol) *sym.Symbol {
	if s.Type != sym.SABIALIAS {
		return s
	}
	target := s.R[0].Sym
	if target.Type == sym.SABIALIAS {
		panic(fmt.Sprintf("ABI alias %s references another ABI alias %s", s, target))
	}
	return target
}

// TODO:
//	generate debugging section in binary.
//	once the dust settles, try to move some code to
//		libmach, so that other linkers and ar can share.

func ldpkg(ctxt *Link, f *bio.Reader, lib *sym.Library, length int64, filename string) {
	if *flagG {
		return
	}

	if int64(int(length)) != length {
		fmt.Fprintf(os.Stderr, "%s: too much pkg data in %s\n", os.Args[0], filename)
		if *flagU {
			errorexit()
		}
		return
	}

	bdata := make([]byte, length)
	if _, err := io.ReadFull(f, bdata); err != nil {
		fmt.Fprintf(os.Stderr, "%s: short pkg read %s\n", os.Args[0], filename)
		if *flagU {
			errorexit()
		}
		return
	}
	data := string(bdata)

	// process header lines
	for data != "" {
		var line string
		if i := strings.Index(data, "\n"); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, ""
		}
		if line == "safe" {
			lib.Safe = true
		}
		if line == "main" {
			lib.Main = true
		}
		if line == "" {
			break
		}
	}

	// look for cgo section
	p0 := strings.Index(data, "\n$$  // cgo")
	var p1 int
	if p0 >= 0 {
		p0 += p1
		i := strings.IndexByte(data[p0+1:], '\n')
		if i < 0 {
			fmt.Fprintf(os.Stderr, "%s: found $$ // cgo but no newline in %s\n", os.Args[0], filename)
			if *flagU {
				errorexit()
			}
			return
		}
		p0 += 1 + i

		p1 = strings.Index(data[p0:], "\n$$")
		if p1 < 0 {
			p1 = strings.Index(data[p0:], "\n!\n")
		}
		if p1 < 0 {
			fmt.Fprintf(os.Stderr, "%s: cannot find end of // cgo section in %s\n", os.Args[0], filename)
			if *flagU {
				errorexit()
			}
			return
		}
		p1 += p0
		loadcgo(ctxt, filename, objabi.PathToPrefix(lib.Pkg), data[p0:p1])
	}
}

func loadcgo(ctxt *Link, file string, pkg string, p string) {
	var directives [][]string
	if err := json.NewDecoder(strings.NewReader(p)).Decode(&directives); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s: failed decoding cgo directives: %v\n", os.Args[0], file, err)
		nerrors++
		return
	}

	// Find cgo_export symbols. They are roots in the deadcode pass.
	for _, f := range directives {
		switch f[0] {
		case "cgo_export_static", "cgo_export_dynamic":
			if len(f) < 2 || len(f) > 3 {
				continue
			}
			local := f[1]
			switch ctxt.BuildMode {
			case BuildModeCShared, BuildModeCArchive, BuildModePlugin:
				if local == "main" {
					continue
				}
			}
			local = expandpkg(local, pkg)
			if f[0] == "cgo_export_static" {
				ctxt.cgo_export_static[local] = true
			} else {
				ctxt.cgo_export_dynamic[local] = true
			}
		}
	}

	// Record the directives. We'll process them later after Symbols are created.
	ctxt.cgodata = append(ctxt.cgodata, cgodata{file, pkg, directives})
}

// Set symbol attributes or flags based on cgo directives.
// Any newly discovered HOSTOBJ syms are added to 'hostObjSyms'.
func setCgoAttr(ctxt *Link, lookup func(string, int) loader.Sym, file string, pkg string, directives [][]string, hostObjSyms map[loader.Sym]struct{}) {
	l := ctxt.loader
	for _, f := range directives {
		switch f[0] {
		case "cgo_import_dynamic":
			if len(f) < 2 || len(f) > 4 {
				break
			}

			local := f[1]
			remote := local
			if len(f) > 2 {
				remote = f[2]
			}
			lib := ""
			if len(f) > 3 {
				lib = f[3]
			}

			if *FlagD {
				fmt.Fprintf(os.Stderr, "%s: %s: cannot use dynamic imports with -d flag\n", os.Args[0], file)
				nerrors++
				return
			}

			if local == "_" && remote == "_" {
				// allow #pragma dynimport _ _ "foo.so"
				// to force a link of foo.so.
				havedynamic = 1

				if ctxt.HeadType == objabi.Hdarwin {
					machoadddynlib(lib, ctxt.LinkMode)
				} else {
					dynlib = append(dynlib, lib)
				}
				continue
			}

			local = expandpkg(local, pkg)
			q := ""
			if i := strings.Index(remote, "#"); i >= 0 {
				remote, q = remote[:i], remote[i+1:]
			}
			s := lookup(local, 0)
			st := l.SymType(s)
			if st == 0 || st == sym.SXREF || st == sym.SBSS || st == sym.SNOPTRBSS || st == sym.SHOSTOBJ {
				l.SetSymDynimplib(s, lib)
				l.SetSymExtname(s, remote)
				l.SetSymDynimpvers(s, q)
				if st != sym.SHOSTOBJ {
					su := l.MakeSymbolUpdater(s)
					su.SetType(sym.SDYNIMPORT)
				} else {
					hostObjSyms[s] = struct{}{}
				}
				havedynamic = 1
			}

			continue

		case "cgo_import_static":
			if len(f) != 2 {
				break
			}
			local := f[1]

			s := lookup(local, 0)
			su := l.MakeSymbolUpdater(s)
			su.SetType(sym.SHOSTOBJ)
			su.SetSize(0)
			hostObjSyms[s] = struct{}{}
			continue

		case "cgo_export_static", "cgo_export_dynamic":
			if len(f) < 2 || len(f) > 3 {
				break
			}
			local := f[1]
			remote := local
			if len(f) > 2 {
				remote = f[2]
			}
			local = expandpkg(local, pkg)

			// The compiler arranges for an ABI0 wrapper
			// to be available for all cgo-exported
			// functions. Link.loadlib will resolve any
			// ABI aliases we find here (since we may not
			// yet know it's an alias).
			s := lookup(local, 0)

			if l.SymType(s) == sym.SHOSTOBJ {
				hostObjSyms[s] = struct{}{}
			}

			switch ctxt.BuildMode {
			case BuildModeCShared, BuildModeCArchive, BuildModePlugin:
				if s == lookup("main", 0) {
					continue
				}
			}

			// export overrides import, for openbsd/cgo.
			// see issue 4878.
			if l.SymDynimplib(s) != "" {
				l.SetSymDynimplib(s, "")
				l.SetSymDynimpvers(s, "")
				l.SetSymExtname(s, "")
				var su *loader.SymbolBuilder
				su = l.MakeSymbolUpdater(s)
				su.SetType(0)
			}

			if !(l.AttrCgoExportStatic(s) || l.AttrCgoExportDynamic(s)) {
				l.SetSymExtname(s, remote)
			} else if l.SymExtname(s) != remote {
				fmt.Fprintf(os.Stderr, "%s: conflicting cgo_export directives: %s as %s and %s\n", os.Args[0], l.SymName(s), l.SymExtname(s), remote)
				nerrors++
				return
			}

			if f[0] == "cgo_export_static" {
				l.SetAttrCgoExportStatic(s, true)
			} else {
				l.SetAttrCgoExportDynamic(s, true)
			}
			continue

		case "cgo_dynamic_linker":
			if len(f) != 2 {
				break
			}

			if *flagInterpreter == "" {
				if interpreter != "" && interpreter != f[1] {
					fmt.Fprintf(os.Stderr, "%s: conflict dynlinker: %s and %s\n", os.Args[0], interpreter, f[1])
					nerrors++
					return
				}

				interpreter = f[1]
			}
			continue

		case "cgo_ldflag":
			if len(f) != 2 {
				break
			}
			ldflag = append(ldflag, f[1])
			continue
		}

		fmt.Fprintf(os.Stderr, "%s: %s: invalid cgo directive: %q\n", os.Args[0], file, f)
		nerrors++
	}
	return
}

var seenlib = make(map[string]bool)

func adddynlib(ctxt *Link, lib string) {
	if seenlib[lib] || ctxt.LinkMode == LinkExternal {
		return
	}
	seenlib[lib] = true

	if ctxt.IsELF {
		s := ctxt.Syms.Lookup(".dynstr", 0)
		if s.Size == 0 {
			Addstring(s, "")
		}
		Elfwritedynent(ctxt, ctxt.Syms.Lookup(".dynamic", 0), DT_NEEDED, uint64(Addstring(s, lib)))
	} else {
		Errorf(nil, "adddynlib: unsupported binary format")
	}
}

func Adddynsym(ctxt *Link, s *sym.Symbol) {
	if s.Dynid >= 0 || ctxt.LinkMode == LinkExternal {
		return
	}

	if ctxt.IsELF {
		elfadddynsym(ctxt, s)
	} else if ctxt.HeadType == objabi.Hdarwin {
		Errorf(s, "adddynsym: missed symbol (Extname=%s)", s.Extname())
	} else if ctxt.HeadType == objabi.Hwindows {
		// already taken care of
	} else {
		Errorf(s, "adddynsym: unsupported binary format")
	}
}

func fieldtrack(ctxt *Link) {
	// record field tracking references
	var buf bytes.Buffer
	for _, s := range ctxt.Syms.Allsym {
		if strings.HasPrefix(s.Name, "go.track.") {
			s.Attr |= sym.AttrSpecial // do not lay out in data segment
			s.Attr |= sym.AttrNotInSymbolTable
			if s.Attr.Reachable() {
				buf.WriteString(s.Name[9:])
				for p := ctxt.Reachparent[s]; p != nil; p = ctxt.Reachparent[p] {
					buf.WriteString("\t")
					buf.WriteString(p.Name)
				}
				buf.WriteString("\n")
			}

			s.Type = sym.SCONST
			s.Value = 0
		}
	}

	if *flagFieldTrack == "" {
		return
	}
	s := ctxt.Syms.ROLookup(*flagFieldTrack, 0)
	if s == nil || !s.Attr.Reachable() {
		return
	}
	s.Type = sym.SDATA
	addstrdata(ctxt, *flagFieldTrack, buf.String())
}

func (ctxt *Link) addexport() {
	// Track undefined external symbols during external link.
	if ctxt.LinkMode == LinkExternal {
		for _, s := range ctxt.Syms.Allsym {
			if !s.Attr.Reachable() || s.Attr.Special() || s.Attr.SubSymbol() {
				continue
			}
			if s.Type != sym.STEXT {
				continue
			}
			for i := range s.R {
				r := &s.R[i]
				if r.Sym != nil && r.Sym.Type == sym.Sxxx {
					r.Sym.Type = sym.SUNDEFEXT
				}
			}
		}
	}

	// TODO(aix)
	if ctxt.HeadType == objabi.Hdarwin || ctxt.HeadType == objabi.Haix {
		return
	}

	for _, exp := range dynexp {
		Adddynsym(ctxt, exp)
	}
	for _, lib := range dynlib {
		adddynlib(ctxt, lib)
	}
}

type Pkg struct {
	mark    bool
	checked bool
	path    string
	impby   []*Pkg
}

var pkgall []*Pkg

func (p *Pkg) cycle() *Pkg {
	if p.checked {
		return nil
	}

	if p.mark {
		nerrors++
		fmt.Printf("import cycle:\n")
		fmt.Printf("\t%s\n", p.path)
		return p
	}

	p.mark = true
	for _, q := range p.impby {
		if bad := q.cycle(); bad != nil {
			p.mark = false
			p.checked = true
			fmt.Printf("\timports %s\n", p.path)
			if bad == p {
				return nil
			}
			return bad
		}
	}

	p.checked = true
	p.mark = false
	return nil
}

func importcycles() {
	for _, p := range pkgall {
		p.cycle()
	}
}

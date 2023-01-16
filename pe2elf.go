package main

import (
	"bufio"
	"bytes"
	"debug/elf"
	"debug/pe"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
)

var verbose bool

func main() {
	inPath := flag.String("i", "", "PE input file")
	outPath := flag.String("o", "out.elf", "ELF output file")
	flag.BoolVar(&verbose, "v", false, "Verbose output")
	flag.Parse()

	log.Default().SetFlags(0)

	peFile, err := pe.Open(*inPath)
	if err != nil {
		log.Fatal(err)
	}
	defer peFile.Close()

	outFile, err := os.Create(*outPath)
	if err != nil {
		log.Fatal(err)
	}
	defer outFile.Close()

	peOpt := peFile.OptionalHeader.(*pe.OptionalHeader32)
	baseVaddr := peOpt.ImageBase
	log.Printf("Base vaddr:   %#x", baseVaddr)

	entryVaddr := baseVaddr + peOpt.AddressOfEntryPoint
	log.Printf("Entry vaddr:  %#x", entryVaddr)

	var writer elfWriter
	if err := writer.init(outFile); err != nil {
		log.Fatal(err)
	}
	writer.hdr.Entry = entryVaddr

	peText := peFile.Section(".text")
	rawText := peText.Open()
	log.Printf("Text vaddr:   %#x", baseVaddr+peText.VirtualAddress)
	if err = writer.copySection(rawText, ".text", elf.Section32{
		Type:  uint32(elf.SHT_PROGBITS),
		Flags: uint32(elf.SHF_ALLOC | elf.SHF_EXECINSTR),
		Addr:  baseVaddr + peText.VirtualAddress,
	}); err != nil {
		log.Fatal(err)
	}

	peExc := peFile.Section(".exc")
	rawExc := peExc.Open()
	log.Printf("Exc vaddr:    %#x", baseVaddr+peExc.VirtualAddress)
	if err = writer.copySection(rawExc, ".rodata.exc", elf.Section32{
		Type:  uint32(elf.SHT_PROGBITS),
		Flags: uint32(elf.SHF_ALLOC),
		Addr:  baseVaddr + peExc.VirtualAddress,
	}); err != nil {
		log.Fatal(err)
	}

	peRodata := peFile.Section(".rdata")
	rawRodata := peRodata.Open()
	log.Printf("Rodata vaddr: %#x", baseVaddr+peRodata.VirtualAddress)
	if err = writer.copySection(rawRodata, ".rodata", elf.Section32{
		Type:  uint32(elf.SHT_PROGBITS),
		Flags: uint32(elf.SHF_ALLOC),
		Addr:  baseVaddr + peRodata.VirtualAddress,
	}); err != nil {
		log.Fatal(err)
	}

	peData := peFile.Section(".data")
	rawData := peData.Open()
	log.Printf("Data vaddr:   %#x", baseVaddr+peData.VirtualAddress)
	if err = writer.copySection(rawData, ".data", elf.Section32{
		Type:  uint32(elf.SHT_PROGBITS),
		Flags: uint32(elf.SHF_ALLOC | elf.SHF_WRITE),
		Addr:  baseVaddr + peData.VirtualAddress,
	}); err != nil {
		log.Fatal(err)
	}

	peCRT := peFile.Section(".CRT")
	rawCRT := peCRT.Open()
	log.Printf("CRT vaddr:    %#x", baseVaddr+peCRT.VirtualAddress)
	if err = writer.copySection(rawCRT, ".data.CRT", elf.Section32{
		Type:  uint32(elf.SHT_PROGBITS),
		Flags: uint32(elf.SHF_ALLOC | elf.SHF_WRITE),
		Addr:  baseVaddr + peCRT.VirtualAddress,
	}); err != nil {
		log.Fatal(err)
	}

	peIdata := peFile.Section(".idata")
	log.Printf("Idata vaddr:  %#x", baseVaddr+peIdata.VirtualAddress)
	// Instead of copying .idata, we zero it out completely.
	// ELF relocs will fill it in, but we don't want any implicit addends.
	// TODO This zero filling could be a bit more graceful
	if err = writer.copySection(
		bytes.NewReader(make([]byte, peIdata.VirtualSize)),
		".data.idata", elf.Section32{
			Type:  uint32(elf.SHT_PROGBITS),
			Flags: uint32(elf.SHF_ALLOC | elf.SHF_WRITE),
			Addr:  baseVaddr + peIdata.VirtualAddress,
		}); err != nil {
		log.Fatal(err)
	}

	idataNdx := len(writer.sections) - 1
	if err = writer.addImports(peFile, peOpt, idataNdx); err != nil {
		log.Fatal(err)
	}

	writer.addImplicitSyms()

	peBss := peFile.Section(".bss")
	log.Printf("Bss vaddr:    %#x", baseVaddr+peBss.VirtualAddress)
	writer.addBss(peBss.VirtualSize, baseVaddr+peBss.VirtualAddress)

	peRelocs := peFile.Section(".reloc")
	writer.addRelocs(peRelocs, baseVaddr)

	if err := writer.finish(); err != nil {
		log.Fatal(err)
	}
}

func getString(section []byte, start int) (string, bool) {
	if start < 0 || start >= len(section) {
		return "", false
	}
	for end := start; end < len(section); end++ {
		if section[end] == 0 {
			return string(section[start:end]), true
		}
	}
	return "", false
}

type symkey struct {
	shndx uint32
	value uint32
}

type elfWriter struct {
	hdr      elf.Header32
	wr       *os.File
	sections []elf.Section32
	symtab   []elf.Sym32
	symmap   map[symkey]int      // (shndx, vaddr) -> index in symtab
	relocs   map[int][]elf.Rel32 // section index -> relocs

	shstrtab bytes.Buffer
	strtab   bytes.Buffer
}

func (e *elfWriter) init(wr *os.File) error {
	e.wr = wr
	e.strtab.WriteByte(0)
	e.shstrtab.WriteByte(0)
	e.hdr = elf.Header32{
		Ident: [elf.EI_NIDENT]byte{
			elf.ELFMAG[0], elf.ELFMAG[1], elf.ELFMAG[2], elf.ELFMAG[3],
			byte(elf.ELFCLASS32),
			byte(elf.ELFDATA2LSB),
			byte(elf.EV_CURRENT),
			0, // ELFOSABI_SYSV
		},
		Type:    uint16(elf.ET_REL),
		Machine: uint16(elf.EM_386),
		Version: uint32(elf.EV_CURRENT),
		Ehsize:  uint16(binary.Size(e.hdr)),
	}
	e.sections = []elf.Section32{{}}
	e.symtab = []elf.Sym32{{}}
	e.symmap = make(map[symkey]int)
	e.relocs = make(map[int][]elf.Rel32)
	return binary.Write(wr, binary.LittleEndian, &e.hdr)
}

func (e *elfWriter) pos() uint32 {
	x, err := e.wr.Seek(0, io.SeekCurrent)
	if err != nil {
		panic(err)
	}
	return uint32(x)
}

func (e *elfWriter) align(n uint32) error {
	pos := e.pos()
	if pos%n != 0 {
		_, err := e.wr.Seek(int64(n-(pos%n)), io.SeekCurrent)
		return err
	}
	return nil
}

func (e *elfWriter) addBss(size uint32, vaddr uint32) {
	e.sections = append(e.sections, elf.Section32{
		Name:  e.addShstr(".bss"),
		Type:  uint32(elf.SHT_NOBITS),
		Flags: uint32(elf.SHF_ALLOC) | uint32(elf.SHF_WRITE),
		Size:  size,
		Addr:  vaddr,
	})
}

func (e *elfWriter) copySection(rd io.Reader, name string, sec elf.Section32) error {
	if err := e.align(0x100); err != nil {
		return err
	}

	atStart := e.pos()
	n, err := io.Copy(e.wr, rd)
	if err != nil {
		return err
	}
	atEnd := atStart + uint32(n)

	sec.Name = e.addShstr(name)
	sec.Off = atStart
	sec.Size = atEnd - atStart
	sec.Addralign = 1
	e.sections = append(e.sections, sec)

	return nil
}

// addShstr adds a string to the section header string table.
func (e *elfWriter) addShstr(s string) uint32 {
	addr := uint32(e.shstrtab.Len())
	e.shstrtab.WriteString(s)
	e.shstrtab.WriteByte(0)
	return addr
}

// addStr adds a string to the string table.
func (e *elfWriter) addStr(s string) uint32 {
	addr := uint32(e.strtab.Len())
	e.strtab.WriteString(s)
	e.strtab.WriteByte(0)
	return addr
}

// addSym adds a symbol to the symbol table.
func (e *elfWriter) addSym(sym elf.Sym32, name string) int {
	key := symkey{
		shndx: uint32(sym.Shndx),
		value: sym.Value,
	}
	if symndx, ok := e.symmap[key]; ok {
		return symndx
	}
	if name != "" {
		sym.Name = e.addStr(name)
	}
	ndx := len(e.symtab)
	e.symtab = append(e.symtab, sym)
	e.symmap[key] = ndx
	return ndx
}

// addRelocs parses PE relocations and emits ELF relocations.
//
// All of this only covers relocations of absolute 32-bit virtual addresses, i.e. R_386_32.
//
// Some terminology:
// - Site: the address that needs to be patched
// - Target: the (virtual) address of the symbol that the site refers to
//
// In PE, this works by simply having the site refer to the target virtual address.
// The relocation table contains a list of these sites.
// Because the site already contains the target address, no further info is required in the reloc itself.
//
// ELF is a bit more flexible, however.
// Patching works by creating a symbol that points at or before the target,
// and then having the reloc associate a site with that symbol.
// An addend, which is sourced from the original site or `r_addend`,
// is added to the symbol's address to get the final target address.
//
// We don't have any actual symbols, so we use the start of the target's as the symbol,
// and store the offset between the target and the symbol in the addend field.
func (e *elfWriter) addRelocs(s *pe.Section, baseVaddr uint32) {
	rd := s.Open()
	for {
		var hdr struct {
			PageRVA   uint32
			BlockSize uint32
		}
		if err := binary.Read(rd, binary.LittleEndian, &hdr); err != nil {
			if err == io.EOF {
				break
			}
			log.Fatal(err)
		}
		if hdr.PageRVA == 0 {
			break
		}
		pageVA := baseVaddr + hdr.PageRVA
		//log.Printf("Reloc block: %#x", hdr.PageRVA)
		for i := uint32(0); i < hdr.BlockSize-8; i += 2 {
			var reloc uint16
			if err := binary.Read(rd, binary.LittleEndian, &reloc); err != nil {
				log.Fatal(err)
			}
			if reloc == 0 {
				break
			}
			relocType := reloc >> 12
			relocOffset := reloc & 0xfff
			if relocType != 3 {
				log.Printf("unsupported reloc type: " + strconv.Itoa(int(relocType)))
				continue
			}
			siteVaddr := pageVA + uint32(relocOffset)

			// Detect section of reloc site
			siteShndx := -1
			for i, s := range e.sections {
				if s.Addr <= siteVaddr && siteVaddr < s.Addr+s.Size {
					siteShndx = i
					break
				}
			}
			if siteShndx < 0 {
				log.Printf("Reloc site of any ELF section (type=%d, vaddr=%#x)", relocType, siteVaddr)
				continue
			}

			// Read original target address
			sitePaddr := e.sections[siteShndx].Off + siteVaddr - e.sections[siteShndx].Addr
			var origAddrBuf [4]byte
			if _, err := e.wr.ReadAt(origAddrBuf[:], int64(sitePaddr)); err != nil {
				log.Fatal(err)
			}
			targetVaddr := binary.LittleEndian.Uint32(origAddrBuf[:])

			// Patch site to zero
			// TODO: We could simplify this by writing a section offset,
			//       and using a symbol that points to the section start.
			if _, err := e.wr.WriteAt([]byte{0, 0, 0, 0}, int64(sitePaddr)); err != nil {
				log.Fatal(err)
			}

			// Detect section of reloc target
			targetShndx := -1
			for i, s := range e.sections {
				if s.Addr <= targetVaddr && targetVaddr < s.Addr+s.Size {
					targetShndx = i
					break
				}
			}
			if targetShndx < 0 {
				log.Printf("Reloc target outside of any ELF section (type=%d, vaddr=%#x)", relocType, targetVaddr)
				continue
			}

			// Create symbol for target
			targetShName, _ := getString(e.shstrtab.Bytes(), int(e.sections[targetShndx].Name))
			targetSymIdx := e.addSym(elf.Sym32{
				Value: targetVaddr - e.sections[targetShndx].Addr,
				Info:  elf.ST_INFO(elf.STB_GLOBAL, elf.STT_NOTYPE),
				Shndx: uint16(targetShndx),
				Other: uint8(elf.STV_DEFAULT),
				Size:  0,
			}, fmt.Sprintf("__pe_unk_%x", targetVaddr))

			// Create relocation entry
			rel := elf.Rel32{
				Off:  siteVaddr - e.sections[siteShndx].Addr,
				Info: elf.R_INFO32(uint32(targetSymIdx), uint32(elf.R_386_32)),
			}
			e.relocs[siteShndx] = append(e.relocs[siteShndx], rel)

			if verbose {
				siteShName, _ := getString(e.shstrtab.Bytes(), int(e.sections[siteShndx].Name))
				log.Printf("Reloc type=%d %#x (%s+%#x) -> %#x (%s+%#x)",
					relocType,
					siteVaddr, siteShName, siteVaddr-e.sections[siteShndx].Addr,
					targetVaddr, targetShName, targetVaddr-e.sections[targetShndx].Addr,
				)
			}
		}
	}
}

func (e *elfWriter) addImplicitSyms() {
	for i, s := range e.sections {
		shName, _ := getString(e.shstrtab.Bytes(), int(s.Name))
		e.addSym(elf.Sym32{
			Value: 0,
			Info:  elf.ST_INFO(elf.STB_GLOBAL, elf.STT_NOTYPE),
			Shndx: uint16(i),
			Other: uint8(elf.STV_DEFAULT),
			Size:  0,
		}, "__pe"+strings.ReplaceAll(shName, ".", "_")+"_start")
	}
}

// writeShstrtab writes the .shstrtab section (section header string table).
func (e *elfWriter) writeShstrtab() error {
	e.hdr.Shstrndx = uint16(len(e.sections))
	selfName := e.addShstr(".shstrtab")

	atStart := e.pos()
	if _, err := io.Copy(e.wr, &e.shstrtab); err != nil {
		return err
	}
	e.shstrtab.Reset()
	atEnd := e.pos()

	sec := elf.Section32{
		Name: selfName,
		Type: uint32(elf.SHT_STRTAB),
		Off:  atStart,
		Size: atEnd - atStart,
	}
	e.sections = append(e.sections, sec)

	return nil
}

// writeStrtab writes the .strtab section (string table).
func (e *elfWriter) writeStrtab() error {
	atStart := e.pos()
	if _, err := io.Copy(e.wr, &e.strtab); err != nil {
		return err
	}
	e.strtab.Reset()
	atEnd := e.pos()

	sec := elf.Section32{
		Name: e.addShstr(".strtab"),
		Type: uint32(elf.SHT_STRTAB),
		Off:  atStart,
		Size: atEnd - atStart,
	}
	e.sections = append(e.sections, sec)

	return nil
}

// writeShtab writes the section header table.
func (e *elfWriter) writeShtab() error {
	atStart := e.pos()
	buf := bufio.NewWriter(e.wr)
	for _, sec := range e.sections {
		if err := binary.Write(buf, binary.LittleEndian, &sec); err != nil {
			return err
		}
	}
	if err := buf.Flush(); err != nil {
		return err
	}

	e.hdr.Shoff = atStart
	e.hdr.Shentsize = uint16(binary.Size(elf.Section32{}))
	e.hdr.Shnum = uint16(len(e.sections))
	return nil
}

func (e *elfWriter) addImports(peFile *pe.File, peOpt *pe.OptionalHeader32, idataNdx int) error {
	idd := peOpt.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_IMPORT]
	var ds *pe.Section
	ds = nil
	for _, s := range peFile.Sections {
		if s.VirtualAddress <= idd.VirtualAddress && idd.VirtualAddress < s.VirtualAddress+s.VirtualSize {
			ds = s
			break
		}
	}
	if ds == nil {
		return fmt.Errorf("could not find section containing import directory table")
	}
	d, err := ds.Data()
	if err != nil {
		return err
	}
	d = d[idd.VirtualAddress-ds.VirtualAddress:]

	// start decoding the import directory
	var ida []pe.ImportDirectory
	for len(d) >= 20 {
		var dt pe.ImportDirectory
		dt.OriginalFirstThunk = binary.LittleEndian.Uint32(d[0:4])
		dt.TimeDateStamp = binary.LittleEndian.Uint32(d[4:8])
		dt.ForwarderChain = binary.LittleEndian.Uint32(d[8:12])
		dt.Name = binary.LittleEndian.Uint32(d[12:16])
		dt.FirstThunk = binary.LittleEndian.Uint32(d[16:20])
		d = d[20:]
		if dt.OriginalFirstThunk == 0 {
			break
		}
		ida = append(ida, dt)
	}
	names, _ := ds.Data()
	dlls := make([]string, len(ida))
	for i, dt := range ida {
		dlls[i], _ = getString(names, int(dt.Name-ds.VirtualAddress))
		d, _ = ds.Data()
		// seek to OriginalFirstThunk
		d = d[dt.OriginalFirstThunk-ds.VirtualAddress:]
		targetAddr := peOpt.ImageBase + dt.FirstThunk
		for len(d) > 0 {
			va := binary.LittleEndian.Uint32(d[0:4])
			d = d[4:]
			if va == 0 {
				break
			}
			fn, _ := getString(names, int(va-ds.VirtualAddress+2))
			if fn == "" {
				fn = fmt.Sprintf("%x", targetAddr)
			}

			// Declare new undefined symbol
			symName := strings.TrimSuffix(dlls[i], ".dll") + "_" + fn
			symIdx := e.addSym(elf.Sym32{
				Value: va,
				Info:  elf.ST_INFO(elf.STB_GLOBAL, elf.STT_FUNC),
				Other: uint8(elf.STV_DEFAULT),
				Shndx: uint16(elf.SHN_UNDEF),
			}, symName)

			// Emit relocation to symbol
			e.relocs[idataNdx] = append(e.relocs[idataNdx], elf.Rel32{
				Off:  targetAddr - e.sections[idataNdx].Addr,
				Info: elf.R_INFO32(uint32(symIdx), uint32(elf.R_386_32)),
			})

			log.Printf("Import at %#x %s!%s as %s", targetAddr, dlls[i], fn, symName)
			targetAddr += 4
		}
	}

	return nil
}

func (e *elfWriter) writeReltabs() error {
	// Count number of reloc sections
	nReltabs := 0
	for i := 0; i < len(e.sections); i++ {
		if len(e.relocs[i]) > 0 {
			nReltabs++
		}
	}

	// Write reloc sections
	buf := bufio.NewWriter(e.wr)
	for i := 0; i < len(e.sections); i++ {
		if len(e.relocs[i]) == 0 {
			continue
		}
		nReltabs--
		atStart := e.pos()
		if err := binary.Write(buf, binary.LittleEndian, e.relocs[i]); err != nil {
			return err
		}
		if err := buf.Flush(); err != nil {
			return err
		}
		atEnd := e.pos()

		// Get target section name
		name, _ := getString(e.shstrtab.Bytes(), int(e.sections[i].Name))
		if name == "" {
			panic("cannot find section name")
		}

		sec := elf.Section32{
			Name:    e.addShstr(".rel" + name),
			Type:    uint32(elf.SHT_REL),
			Off:     atStart,
			Size:    atEnd - atStart,
			Link:    uint32(len(e.sections) + nReltabs + 1), // .symtab follows last reloc section
			Info:    uint32(i),
			Entsize: uint32(binary.Size(elf.Rel32{})),
		}
		e.sections = append(e.sections, sec)
	}

	return nil
}

func (e *elfWriter) writeSymtab() error {
	atStart := e.pos()
	buf := bufio.NewWriter(e.wr)
	binary.Write(buf, binary.LittleEndian, e.symtab)
	if err := buf.Flush(); err != nil {
		return err
	}
	atEnd := e.pos()

	sec := elf.Section32{
		Name:    e.addShstr(".symtab"),
		Type:    uint32(elf.SHT_SYMTAB),
		Off:     atStart,
		Size:    atEnd - atStart,
		Entsize: uint32(binary.Size(elf.Sym32{})),
		Link:    uint32(len(e.sections) + 1), // .strtab follows symtab
		Info:    1,
	}
	e.sections = append(e.sections, sec)

	return nil
}

func (e *elfWriter) finish() error {
	if err := e.writeReltabs(); err != nil {
		return err
	}
	if err := e.writeSymtab(); err != nil {
		return err
	}
	if err := e.writeStrtab(); err != nil {
		return err
	}
	if err := e.writeShstrtab(); err != nil {
		return err
	}
	if err := e.writeShtab(); err != nil {
		return err
	}
	if _, err := e.wr.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return binary.Write(e.wr, binary.LittleEndian, e.hdr)
}

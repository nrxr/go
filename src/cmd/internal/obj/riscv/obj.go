// Copyright © 2015 The Go Authors.  All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.  IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package riscv

import (
	"cmd/internal/obj"
	"cmd/internal/sys"
	"fmt"
)

// TODO(jsing): Populate.
var RISCV64DWARFRegisters = map[int16]int16{}

func buildop(ctxt *obj.Link) {}

// lowerJALR normalizes a JALR instruction.
func lowerJALR(p *obj.Prog) {
	if p.As != AJALR {
		panic("lowerJALR: not a JALR")
	}

	// JALR gets parsed like JAL - the linkage pointer goes in From,
	// and the target is in To. However, we need to assemble it as an
	// I-type instruction, so place the linkage pointer in To, the
	// target register in Reg, and the offset in From.
	p.Reg = p.To.Reg
	p.From, p.To = p.To, p.From
	p.From.Type, p.From.Reg = obj.TYPE_CONST, obj.REG_NONE
}

// progedit is called individually for each *obj.Prog. It normalizes instruction
// formats and eliminates as many pseudo-instructions as possible.
func progedit(ctxt *obj.Link, p *obj.Prog, newprog obj.ProgAlloc) {

	// Expand binary instructions to ternary ones.
	if p.Reg == 0 {
		switch p.As {
		case AADDI, ASLTI, ASLTIU, AANDI, AORI, AXORI, ASLLI, ASRLI, ASRAI,
			AADD, AAND, AOR, AXOR, ASLL, ASRL, ASUB, ASRA:
			p.Reg = p.To.Reg
		}
	}

	// Rewrite instructions with constant operands to refer to the immediate
	// form of the instruction.
	if p.From.Type == obj.TYPE_CONST {
		switch p.As {
		case AADD:
			p.As = AADDI
		case ASLT:
			p.As = ASLTI
		case ASLTU:
			p.As = ASLTIU
		case AAND:
			p.As = AANDI
		case AOR:
			p.As = AORI
		case AXOR:
			p.As = AXORI
		case ASLL:
			p.As = ASLLI
		case ASRL:
			p.As = ASRLI
		case ASRA:
			p.As = ASRAI
		}
	}

	switch p.As {
	case ALW, ALWU, ALH, ALHU, ALB, ALBU, ALD, AFLW, AFLD:
		switch p.From.Type {
		case obj.TYPE_MEM:
			// Convert loads from memory/addresses to ternary form.
			p.Reg = p.From.Reg
			p.From.Type, p.From.Reg = obj.TYPE_CONST, obj.REG_NONE
		default:
			p.Ctxt.Diag("%v\tmemory required for source", p)
		}

	case ASW, ASH, ASB, ASD, AFSW, AFSD:
		switch p.To.Type {
		case obj.TYPE_MEM:
			// Convert stores to memory/addresses to ternary form.
			p.Reg = p.From.Reg
			p.From.Type, p.From.Offset, p.From.Reg = obj.TYPE_CONST, p.To.Offset, obj.REG_NONE
			p.To.Type, p.To.Offset = obj.TYPE_REG, 0
		default:
			p.Ctxt.Diag("%v\tmemory required for destination", p)
		}

	case AJALR:
		lowerJALR(p)

	case obj.AUNDEF, AECALL, AEBREAK, ASCALL, ASBREAK, ARDCYCLE, ARDTIME, ARDINSTRET:
		switch p.As {
		case obj.AUNDEF:
			p.As = AEBREAK
		case ASCALL:
			// SCALL is the old name for ECALL.
			p.As = AECALL
		case ASBREAK:
			// SBREAK is the old name for EBREAK.
			p.As = AEBREAK
		}

		ins := encode(p.As)
		if ins == nil {
			panic("progedit: tried to rewrite nonexistent instruction")
		}

		// The CSR isn't exactly an offset, but it winds up in the
		// immediate area of the encoded instruction, so record it in
		// the Offset field.
		p.From.Type = obj.TYPE_CONST
		p.From.Offset = ins.csr
		p.Reg = REG_ZERO
		if p.To.Type == obj.TYPE_NONE {
			p.To.Type, p.To.Reg = obj.TYPE_REG, REG_ZERO
		}

	case AFSQRTS, AFSQRTD:
		// These instructions expect a zero (i.e. float register 0)
		// to be the second input operand.
		p.Reg = p.From.Reg
		p.From = obj.Addr{Type: obj.TYPE_REG, Reg: REG_F0}

	case AFCVTWS, AFCVTLS, AFCVTWUS, AFCVTLUS, AFCVTWD, AFCVTLD, AFCVTWUD, AFCVTLUD:
		// Set the rounding mode in funct3 to round to zero.
		p.Scond = 1
	}
}

// setPCs sets the Pc field in all instructions reachable from p.
// It uses pc as the initial value.
func setPCs(p *obj.Prog, pc int64) {
	for ; p != nil; p = p.Link {
		p.Pc = pc
		pc += int64(encodingForProg(p).length)
	}
}

func preprocess(ctxt *obj.Link, cursym *obj.LSym, newprog obj.ProgAlloc) {
	if cursym.Func.Text == nil || cursym.Func.Text.Link == nil {
		return
	}

	text := cursym.Func.Text
	if text.As != obj.ATEXT {
		ctxt.Diag("preprocess: found symbol that does not start with TEXT directive")
		return
	}

	stacksize := text.To.Offset
	if stacksize == -8 {
		// Historical way to mark NOFRAME.
		text.From.Sym.Set(obj.AttrNoFrame, true)
		stacksize = 0
	}
	if stacksize < 0 {
		ctxt.Diag("negative frame size %d - did you mean NOFRAME?", stacksize)
	}
	if text.From.Sym.NoFrame() {
		if stacksize != 0 {
			ctxt.Diag("NOFRAME functions must have a frame size of 0, not %d", stacksize)
		}
	}

	cursym.Func.Args = text.To.Val.(int32)
	cursym.Func.Locals = int32(stacksize)

	// TODO(jsing): Implement.

	setPCs(cursym.Func.Text, 0)

	// Resolve branch and jump targets.
	for p := cursym.Func.Text; p != nil; p = p.Link {
		switch p.As {
		case AJAL, ABEQ, ABNE, ABLT, ABLTU, ABGE, ABGEU:
			switch p.To.Type {
			case obj.TYPE_BRANCH:
				p.To.Type, p.To.Offset = obj.TYPE_CONST, p.Pcond.Pc-p.Pc
			case obj.TYPE_MEM:
				panic("unhandled type")
			}
		}
	}

	// Validate all instructions - this provides nice error messages.
	for p := cursym.Func.Text; p != nil; p = p.Link {
		encodingForProg(p).validate(p)
	}
}

func regVal(r, min, max int16) uint32 {
	if r < min || r > max {
		panic(fmt.Sprintf("register out of range, want %d < %d < %d", min, r, max))
	}
	return uint32(r - min)
}

// regI returns an integer register.
func regI(r int16) uint32 {
	return regVal(r, REG_X0, REG_X31)
}

// regF returns a float register.
func regF(r int16) uint32 {
	return regVal(r, REG_F0, REG_F31)
}

// regAddr extracts a register from an Addr.
func regAddr(a obj.Addr, min, max int16) uint32 {
	if a.Type != obj.TYPE_REG {
		panic(fmt.Sprintf("ill typed: %+v", a))
	}
	return regVal(a.Reg, min, max)
}

// regIAddr extracts the integer register from an Addr.
func regIAddr(a obj.Addr) uint32 {
	return regAddr(a, REG_X0, REG_X31)
}

// regFAddr extracts the float register from an Addr.
func regFAddr(a obj.Addr) uint32 {
	return regAddr(a, REG_F0, REG_F31)
}

// immIFits reports whether immediate value x fits in nbits bits
// as a signed integer.
func immIFits(x int64, nbits uint) bool {
	nbits--
	var min int64 = -1 << nbits
	var max int64 = 1<<nbits - 1
	return min <= x && x <= max
}

// immUFits reports whether immediate value x fits in nbits bits
// as an unsigned integer.
func immUFits(x int64, nbits uint) bool {
	var max int64 = 1<<nbits - 1
	return 0 <= x && x <= max
}

// immI extracts the signed integer literal of the specified size from an Addr.
func immI(a obj.Addr, nbits uint) uint32 {
	if a.Type != obj.TYPE_CONST {
		panic(fmt.Sprintf("ill typed: %+v", a))
	}
	if !immIFits(a.Offset, nbits) {
		panic(fmt.Sprintf("signed immediate %d in %v cannot fit in %d bits", a.Offset, a, nbits))
	}
	return uint32(a.Offset)
}

// immU extracts the unsigned integer literal of the specified size from an Addr.
func immU(a obj.Addr, nbits uint) uint32 {
	if a.Type != obj.TYPE_CONST {
		panic(fmt.Sprintf("ill typed: %+v", a))
	}
	if !immUFits(a.Offset, nbits) {
		panic(fmt.Sprintf("unsigned immediate %d in %v cannot fit in %d bits", a.Offset, a, nbits))
	}
	return uint32(a.Offset)
}

func wantImmI(p *obj.Prog, pos string, a obj.Addr, nbits uint) {
	if a.Type != obj.TYPE_CONST {
		p.Ctxt.Diag("%v\texpected immediate in %s position but got %s", p, pos, obj.Dconv(p, &a))
		return
	}
	if !immIFits(a.Offset, nbits) {
		p.Ctxt.Diag("%v\tsigned immediate in %s position cannot be larger than %d bits but got %d", p, pos, nbits, a.Offset)
	}
}

func wantImmU(p *obj.Prog, pos string, a obj.Addr, nbits uint) {
	if a.Type != obj.TYPE_CONST {
		p.Ctxt.Diag("%v\texpected immediate in %s position but got %s", p, pos, obj.Dconv(p, &a))
		return
	}
	if !immUFits(a.Offset, nbits) {
		p.Ctxt.Diag("%v\tunsigned immediate in %s position cannot be larger than %d bits but got %d", p, pos, nbits, a.Offset)
	}
}

func wantReg(p *obj.Prog, pos string, descr string, r, min, max int16) {
	if r < min || r > max {
		p.Ctxt.Diag("%v\texpected %s register in %s position but got non-%s register %s", p, descr, pos, descr, regName(int(r)))
	}
}

// wantIntReg checks that r is an integer register.
func wantIntReg(p *obj.Prog, pos string, r int16) {
	wantReg(p, pos, "integer", r, REG_X0, REG_X31)
}

// wantFloatReg checks that r is a floating-point register.
func wantFloatReg(p *obj.Prog, pos string, r int16) {
	wantReg(p, pos, "float", r, REG_F0, REG_F31)
}

func wantRegAddr(p *obj.Prog, pos string, a *obj.Addr, descr string, min int16, max int16) {
	if a == nil {
		p.Ctxt.Diag("%v\texpected register in %s position but got nothing", p, pos)
		return
	}
	if a.Type != obj.TYPE_REG {
		p.Ctxt.Diag("%v\texpected register in %s position but got %s", p, pos, obj.Dconv(p, a))
		return
	}
	if a.Reg < min || a.Reg > max {
		p.Ctxt.Diag("%v\texpected %s register in %s position but got non-%s register %s", p, descr, pos, descr, obj.Dconv(p, a))
	}
}

// wantIntRegAddr checks that a contains an integer register.
func wantIntRegAddr(p *obj.Prog, pos string, a *obj.Addr) {
	wantRegAddr(p, pos, a, "integer", REG_X0, REG_X31)
}

// wantFloatRegAddr checks that a contains a floating-point register.
func wantFloatRegAddr(p *obj.Prog, pos string, a *obj.Addr) {
	wantRegAddr(p, pos, a, "float", REG_F0, REG_F31)
}

// wantEvenJumpOffset checks that the jump offset is a multiple of two.
func wantEvenJumpOffset(p *obj.Prog) {
	if p.To.Offset%1 != 0 {
		p.Ctxt.Diag("%v\tjump offset %v must be even", p, obj.Dconv(p, &p.To))
	}
}

func validateRIII(p *obj.Prog) {
	wantIntRegAddr(p, "from", &p.From)
	wantIntReg(p, "reg", p.Reg)
	wantIntRegAddr(p, "to", &p.To)
}

func validateRFFF(p *obj.Prog) {
	wantFloatRegAddr(p, "from", &p.From)
	wantFloatReg(p, "reg", p.Reg)
	wantFloatRegAddr(p, "to", &p.To)
}

func validateRFFI(p *obj.Prog) {
	wantFloatRegAddr(p, "from", &p.From)
	wantFloatReg(p, "reg", p.Reg)
	wantIntRegAddr(p, "to", &p.To)
}

func validateRFI(p *obj.Prog) {
	wantFloatRegAddr(p, "from", &p.From)
	wantIntRegAddr(p, "to", &p.To)
}

func validateRIF(p *obj.Prog) {
	wantIntRegAddr(p, "from", &p.From)
	wantFloatRegAddr(p, "to", &p.To)
}

func validateRFF(p *obj.Prog) {
	wantFloatRegAddr(p, "from", &p.From)
	wantFloatRegAddr(p, "to", &p.To)
}

func validateII(p *obj.Prog) {
	wantImmI(p, "from", p.From, 12)
	wantIntReg(p, "reg", p.Reg)
	wantIntRegAddr(p, "to", &p.To)
}

func validateIF(p *obj.Prog) {
	wantImmI(p, "from", p.From, 12)
	wantIntReg(p, "reg", p.Reg)
	wantFloatRegAddr(p, "to", &p.To)
}

func validateSI(p *obj.Prog) {
	wantImmI(p, "from", p.From, 12)
	wantIntReg(p, "reg", p.Reg)
	wantIntRegAddr(p, "to", &p.To)
}

func validateSF(p *obj.Prog) {
	wantImmI(p, "from", p.From, 12)
	wantFloatReg(p, "reg", p.Reg)
	wantIntRegAddr(p, "to", &p.To)
}

func validateB(p *obj.Prog) {
	// Offsets are multiples of two, so accept 13 bit immediates for the
	// 12 bit slot. We implicitly drop the least significant bit in encodeB.
	wantEvenJumpOffset(p)
	wantImmI(p, "to", p.To, 13)
	wantIntReg(p, "reg", p.Reg)
	wantIntRegAddr(p, "from", &p.From)
}

func validateU(p *obj.Prog) {
	wantImmU(p, "from", p.From, 20)
	wantIntRegAddr(p, "to", &p.To)
}

func validateJ(p *obj.Prog) {
	// Offsets are multiples of two, so accept 21 bit immediates for the
	// 20 bit slot. We implicitly drop the least significant bit in encodeJ.
	wantEvenJumpOffset(p)
	wantImmI(p, "to", p.To, 21)
	wantIntRegAddr(p, "from", &p.From)
}

func validateRaw(p *obj.Prog) {
	// Treat the raw value specially as a 32-bit unsigned integer.
	// Nobody wants to enter negative machine code.
	a := p.From
	if a.Type != obj.TYPE_CONST {
		p.Ctxt.Diag("%v\texpected immediate in raw position but got %s", p, obj.Dconv(p, &a))
		return
	}
	if a.Offset < 0 || 1<<32 <= a.Offset {
		p.Ctxt.Diag("%v\timmediate in raw position cannot be larger than 32 bits but got %d", p, a.Offset)
	}
}

// encodeR encodes an R-type RISC-V instruction.
func encodeR(p *obj.Prog, rs1 uint32, rs2 uint32, rd uint32) uint32 {
	ins := encode(p.As)
	if ins == nil {
		panic("encodeR: could not encode instruction")
	}
	if ins.rs2 != 0 && rs2 != 0 {
		panic("encodeR: instruction uses rs2, but rs2 was nonzero")
	}

	// Use Scond for the floating-point rounding mode override.
	// TODO(sorear): Is there a more appropriate way to handle opcode extension bits like this?
	return ins.funct7<<25 | ins.rs2<<20 | rs2<<20 | rs1<<15 | ins.funct3<<12 | uint32(p.Scond)<<12 | rd<<7 | ins.opcode
}

func encodeRIII(p *obj.Prog) uint32 {
	return encodeR(p, regI(p.Reg), regIAddr(p.From), regIAddr(p.To))
}

func encodeRFFF(p *obj.Prog) uint32 {
	return encodeR(p, regF(p.Reg), regFAddr(p.From), regFAddr(p.To))
}

func encodeRFFI(p *obj.Prog) uint32 {
	return encodeR(p, regF(p.Reg), regFAddr(p.From), regIAddr(p.To))
}

func encodeRFI(p *obj.Prog) uint32 {
	return encodeR(p, regFAddr(p.From), 0, regIAddr(p.To))
}

func encodeRIF(p *obj.Prog) uint32 {
	return encodeR(p, regIAddr(p.From), 0, regFAddr(p.To))
}

func encodeRFF(p *obj.Prog) uint32 {
	return encodeR(p, regFAddr(p.From), 0, regFAddr(p.To))
}

// encodeI encodes an I-type RISC-V instruction.
func encodeI(p *obj.Prog, rd uint32) uint32 {
	imm := immI(p.From, 12)
	rs1 := regI(p.Reg)
	ins := encode(p.As)
	if ins == nil {
		panic("encodeI: could not encode instruction")
	}
	imm |= uint32(ins.csr)
	return imm<<20 | rs1<<15 | ins.funct3<<12 | rd<<7 | ins.opcode
}

func encodeII(p *obj.Prog) uint32 {
	return encodeI(p, regIAddr(p.To))
}

func encodeIF(p *obj.Prog) uint32 {
	return encodeI(p, regFAddr(p.To))
}

// encodeS encodes an S-type RISC-V instruction.
func encodeS(p *obj.Prog, rs2 uint32) uint32 {
	imm := immI(p.From, 12)
	rs1 := regIAddr(p.To)
	ins := encode(p.As)
	if ins == nil {
		panic("encodeS: could not encode instruction")
	}
	return (imm>>5)<<25 | rs2<<20 | rs1<<15 | ins.funct3<<12 | (imm&0x1f)<<7 | ins.opcode
}

func encodeSI(p *obj.Prog) uint32 {
	return encodeS(p, regI(p.Reg))
}

func encodeSF(p *obj.Prog) uint32 {
	return encodeS(p, regF(p.Reg))
}

// encodeB encodes a B-type RISC-V instruction.
func encodeB(p *obj.Prog) uint32 {
	imm := immI(p.To, 13)
	rs2 := regI(p.Reg)
	rs1 := regIAddr(p.From)
	ins := encode(p.As)
	if ins == nil {
		panic("encodeB: could not encode instruction")
	}
	return (imm>>12)<<31 | ((imm>>5)&0x3f)<<25 | rs2<<20 | rs1<<15 | ins.funct3<<12 | ((imm>>1)&0xf)<<8 | ((imm>>11)&0x1)<<7 | ins.opcode
}

// encodeU encodes a U-type RISC-V instruction.
func encodeU(p *obj.Prog) uint32 {
	// The immediates for encodeU are the upper 20 bits of a 32 bit value.
	// Rather than have the user/compiler generate a 32 bit constant, the
	// bottommost bits of which must all be zero, instead accept just the
	// top bits.
	imm := immU(p.From, 20)
	rd := regIAddr(p.To)
	ins := encode(p.As)
	if ins == nil {
		panic("encodeU: could not encode instruction")
	}
	return imm<<12 | rd<<7 | ins.opcode
}

// encodeJ encodes a J-type RISC-V instruction.
func encodeJ(p *obj.Prog) uint32 {
	imm := immI(p.To, 21)
	rd := regIAddr(p.From)
	ins := encode(p.As)
	if ins == nil {
		panic("encodeJ: could not encode instruction")
	}
	return (imm>>20)<<31 | ((imm>>1)&0x3ff)<<21 | ((imm>>11)&0x1)<<20 | ((imm>>12)&0xff)<<12 | rd<<7 | ins.opcode
}

// encodeRaw encodes a raw instruction value.
func encodeRaw(p *obj.Prog) uint32 {
	// Treat the raw value specially as a 32-bit unsigned integer.
	// Nobody wants to enter negative machine code.
	a := p.From
	if a.Type != obj.TYPE_CONST {
		panic(fmt.Sprintf("ill typed: %+v", a))
	}
	if a.Offset < 0 || 1<<32 <= a.Offset {
		panic(fmt.Sprintf("immediate %d in %v cannot fit in 32 bits", a.Offset, a))
	}
	return uint32(a.Offset)
}

type encoding struct {
	encode   func(*obj.Prog) uint32 // encode returns the machine code for an *obj.Prog
	validate func(*obj.Prog)        // validate validates an *obj.Prog, calling ctxt.Diag for any issues
	length   int                    // length of encoded instruction; 0 for pseudo-ops, 4 otherwise
}

var (
	// Encodings have the following naming convention:
	//
	//  1. the instruction encoding (R/I/S/B/U/J), in lowercase
	//  2. zero or more register operand identifiers (I = integer
	//     register, F = float register), in uppercase
	//  3. the word "Encoding"
	//
	// For example, rIIIEncoding indicates an R-type instruction with two
	// integer register inputs and an integer register output; sFEncoding
	// indicates an S-type instruction with rs2 being a float register.

	rIIIEncoding = encoding{encode: encodeRIII, validate: validateRIII, length: 4}
	rFFFEncoding = encoding{encode: encodeRFFF, validate: validateRFFF, length: 4}
	rFFIEncoding = encoding{encode: encodeRFFI, validate: validateRFFI, length: 4}
	rFIEncoding  = encoding{encode: encodeRFI, validate: validateRFI, length: 4}
	rIFEncoding  = encoding{encode: encodeRIF, validate: validateRIF, length: 4}
	rFFEncoding  = encoding{encode: encodeRFF, validate: validateRFF, length: 4}

	iIEncoding = encoding{encode: encodeII, validate: validateII, length: 4}
	iFEncoding = encoding{encode: encodeIF, validate: validateIF, length: 4}

	sIEncoding = encoding{encode: encodeSI, validate: validateSI, length: 4}
	sFEncoding = encoding{encode: encodeSF, validate: validateSF, length: 4}

	bEncoding = encoding{encode: encodeB, validate: validateB, length: 4}
	uEncoding = encoding{encode: encodeU, validate: validateU, length: 4}
	jEncoding = encoding{encode: encodeJ, validate: validateJ, length: 4}

	// rawEncoding encodes a raw instruction byte sequence.
	rawEncoding = encoding{encode: encodeRaw, validate: validateRaw, length: 4}

	// pseudoOpEncoding panics if encoding is attempted, but does no validation.
	pseudoOpEncoding = encoding{encode: nil, validate: func(*obj.Prog) {}, length: 0}

	// badEncoding is used when an invalid op is encountered.
	// An error has already been generated, so let anything else through.
	badEncoding = encoding{encode: func(*obj.Prog) uint32 { return 0 }, validate: func(*obj.Prog) {}, length: 0}
)

// encodingForAs contains the encoding for a RISC-V instruction.
// Instructions are masked with obj.AMask to keep indices small.
var encodingForAs = [ALAST & obj.AMask]encoding{

	// Unprivileged ISA

	// 2.4: Integer Computational Instructions
	AADDI & obj.AMask:  iIEncoding,
	ASLTI & obj.AMask:  iIEncoding,
	ASLTIU & obj.AMask: iIEncoding,
	AANDI & obj.AMask:  iIEncoding,
	AORI & obj.AMask:   iIEncoding,
	AXORI & obj.AMask:  iIEncoding,
	ASLLI & obj.AMask:  iIEncoding,
	ASRLI & obj.AMask:  iIEncoding,
	ASRAI & obj.AMask:  iIEncoding,
	ALUI & obj.AMask:   uEncoding,
	AAUIPC & obj.AMask: uEncoding,
	AADD & obj.AMask:   rIIIEncoding,
	ASLT & obj.AMask:   rIIIEncoding,
	ASLTU & obj.AMask:  rIIIEncoding,
	AAND & obj.AMask:   rIIIEncoding,
	AOR & obj.AMask:    rIIIEncoding,
	AXOR & obj.AMask:   rIIIEncoding,
	ASLL & obj.AMask:   rIIIEncoding,
	ASRL & obj.AMask:   rIIIEncoding,
	ASUB & obj.AMask:   rIIIEncoding,
	ASRA & obj.AMask:   rIIIEncoding,

	// 2.5: Control Transfer Instructions
	AJAL & obj.AMask:  jEncoding,
	AJALR & obj.AMask: iIEncoding,
	ABEQ & obj.AMask:  bEncoding,
	ABNE & obj.AMask:  bEncoding,
	ABLT & obj.AMask:  bEncoding,
	ABLTU & obj.AMask: bEncoding,
	ABGE & obj.AMask:  bEncoding,
	ABGEU & obj.AMask: bEncoding,

	// 2.6: Load and Store Instructions
	ALW & obj.AMask:  iIEncoding,
	ALWU & obj.AMask: iIEncoding,
	ALH & obj.AMask:  iIEncoding,
	ALHU & obj.AMask: iIEncoding,
	ALB & obj.AMask:  iIEncoding,
	ALBU & obj.AMask: iIEncoding,
	ASW & obj.AMask:  sIEncoding,
	ASH & obj.AMask:  sIEncoding,
	ASB & obj.AMask:  sIEncoding,

	// 5.2: Integer Computational Instructions (RV64I)
	AADDIW & obj.AMask: iIEncoding,
	ASLLIW & obj.AMask: iIEncoding,
	ASRLIW & obj.AMask: iIEncoding,
	ASRAIW & obj.AMask: iIEncoding,
	AADDW & obj.AMask:  rIIIEncoding,
	ASLLW & obj.AMask:  rIIIEncoding,
	ASRLW & obj.AMask:  rIIIEncoding,
	ASUBW & obj.AMask:  rIIIEncoding,
	ASRAW & obj.AMask:  rIIIEncoding,

	// 5.3: Load and Store Instructions (RV64I)
	ALD & obj.AMask: iIEncoding,
	ASD & obj.AMask: sIEncoding,

	// 7.1: Multiplication Operations
	AMUL & obj.AMask:    rIIIEncoding,
	AMULH & obj.AMask:   rIIIEncoding,
	AMULHU & obj.AMask:  rIIIEncoding,
	AMULHSU & obj.AMask: rIIIEncoding,
	AMULW & obj.AMask:   rIIIEncoding,
	ADIV & obj.AMask:    rIIIEncoding,
	ADIVU & obj.AMask:   rIIIEncoding,
	AREM & obj.AMask:    rIIIEncoding,
	AREMU & obj.AMask:   rIIIEncoding,
	ADIVW & obj.AMask:   rIIIEncoding,
	ADIVUW & obj.AMask:  rIIIEncoding,
	AREMW & obj.AMask:   rIIIEncoding,
	AREMUW & obj.AMask:  rIIIEncoding,

	// 10.1: Base Counters and Timers
	ARDCYCLE & obj.AMask:   iIEncoding,
	ARDTIME & obj.AMask:    iIEncoding,
	ARDINSTRET & obj.AMask: iIEncoding,

	// 11.5: Single-Precision Load and Store Instructions
	AFLW & obj.AMask: iFEncoding,
	AFSW & obj.AMask: sFEncoding,

	// 11.6: Single-Precision Floating-Point Computational Instructions
	AFADDS & obj.AMask:  rFFFEncoding,
	AFSUBS & obj.AMask:  rFFFEncoding,
	AFMULS & obj.AMask:  rFFFEncoding,
	AFDIVS & obj.AMask:  rFFFEncoding,
	AFMINS & obj.AMask:  rFFFEncoding,
	AFMAXS & obj.AMask:  rFFFEncoding,
	AFSQRTS & obj.AMask: rFFFEncoding,

	// 11.7: Single-Precision Floating-Point Conversion and Move Instructions
	AFCVTWS & obj.AMask:  rFIEncoding,
	AFCVTLS & obj.AMask:  rFIEncoding,
	AFCVTSW & obj.AMask:  rIFEncoding,
	AFCVTSL & obj.AMask:  rIFEncoding,
	AFCVTWUS & obj.AMask: rFIEncoding,
	AFCVTLUS & obj.AMask: rFIEncoding,
	AFCVTSWU & obj.AMask: rIFEncoding,
	AFCVTSLU & obj.AMask: rIFEncoding,
	AFSGNJS & obj.AMask:  rFFFEncoding,
	AFSGNJNS & obj.AMask: rFFFEncoding,
	AFSGNJXS & obj.AMask: rFFFEncoding,
	AFMVXS & obj.AMask:   rFIEncoding,
	AFMVSX & obj.AMask:   rIFEncoding,
	AFMVXW & obj.AMask:   rFIEncoding,
	AFMVWX & obj.AMask:   rIFEncoding,

	// 11.8: Single-Precision Floating-Point Compare Instructions
	AFEQS & obj.AMask: rFFIEncoding,
	AFLTS & obj.AMask: rFFIEncoding,
	AFLES & obj.AMask: rFFIEncoding,

	// 12.3: Double-Precision Load and Store Instructions
	AFLD & obj.AMask: iFEncoding,
	AFSD & obj.AMask: sFEncoding,

	// 12.4: Double-Precision Floating-Point Computational Instructions
	AFADDD & obj.AMask:  rFFFEncoding,
	AFSUBD & obj.AMask:  rFFFEncoding,
	AFMULD & obj.AMask:  rFFFEncoding,
	AFDIVD & obj.AMask:  rFFFEncoding,
	AFMIND & obj.AMask:  rFFFEncoding,
	AFMAXD & obj.AMask:  rFFFEncoding,
	AFSQRTD & obj.AMask: rFFFEncoding,

	// 12.5: Double-Precision Floating-Point Conversion and Move Instructions
	AFCVTWD & obj.AMask:  rFIEncoding,
	AFCVTLD & obj.AMask:  rFIEncoding,
	AFCVTDW & obj.AMask:  rIFEncoding,
	AFCVTDL & obj.AMask:  rIFEncoding,
	AFCVTWUD & obj.AMask: rFIEncoding,
	AFCVTLUD & obj.AMask: rFIEncoding,
	AFCVTDWU & obj.AMask: rIFEncoding,
	AFCVTDLU & obj.AMask: rIFEncoding,
	AFCVTSD & obj.AMask:  rFFEncoding,
	AFCVTDS & obj.AMask:  rFFEncoding,
	AFSGNJD & obj.AMask:  rFFFEncoding,
	AFSGNJND & obj.AMask: rFFFEncoding,
	AFSGNJXD & obj.AMask: rFFFEncoding,
	AFMVXD & obj.AMask:   rFIEncoding,
	AFMVDX & obj.AMask:   rIFEncoding,

	// 12.6: Double-Precision Floating-Point Compare Instructions
	AFEQD & obj.AMask: rFFIEncoding,
	AFLTD & obj.AMask: rFFIEncoding,
	AFLED & obj.AMask: rFFIEncoding,

	// Privileged ISA

	// 3.2.1: Environment Call and Breakpoint
	AECALL & obj.AMask:  iIEncoding,
	AEBREAK & obj.AMask: iIEncoding,

	// Escape hatch
	AWORD & obj.AMask: rawEncoding,

	// Pseudo-operations
	obj.AFUNCDATA: pseudoOpEncoding,
	obj.APCDATA:   pseudoOpEncoding,
	obj.ATEXT:     pseudoOpEncoding,
	obj.ANOP:      pseudoOpEncoding,
}

// encodingForProg returns the encoding (encode+validate funcs) for an *obj.Prog.
func encodingForProg(p *obj.Prog) encoding {
	if base := p.As &^ obj.AMask; base != obj.ABaseRISCV && base != 0 {
		p.Ctxt.Diag("encodingForProg: not a RISC-V instruction %s", p.As)
		return badEncoding
	}
	as := p.As & obj.AMask
	if int(as) >= len(encodingForAs) {
		p.Ctxt.Diag("encodingForProg: bad RISC-V instruction %s", p.As)
		return badEncoding
	}
	enc := encodingForAs[as]
	if enc.validate == nil {
		p.Ctxt.Diag("encodingForProg: no encoding for instruction %s", p.As)
		return badEncoding
	}
	return enc
}

// assemble emits machine code.
// It is called at the very end of the assembly process.
func assemble(ctxt *obj.Link, cursym *obj.LSym, newprog obj.ProgAlloc) {
	var symcode []uint32
	for p := cursym.Func.Text; p != nil; p = p.Link {
		enc := encodingForProg(p)
		if enc.length > 0 {
			symcode = append(symcode, enc.encode(p))
		}
	}
	cursym.Size = int64(4 * len(symcode))

	cursym.Grow(cursym.Size)
	for p, i := cursym.P, 0; i < len(symcode); p, i = p[4:], i+1 {
		ctxt.Arch.ByteOrder.PutUint32(p, symcode[i])
	}
}

var LinkRISCV64 = obj.LinkArch{
	Arch:           sys.ArchRISCV64,
	Init:           buildop,
	Preprocess:     preprocess,
	Assemble:       assemble,
	Progedit:       progedit,
	UnaryDst:       unaryDst,
	DWARFRegisters: RISCV64DWARFRegisters,
}

// Copyright (c) 2015-2021, NVIDIA CORPORATION.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io/ioutil"
	"os"
)

const bytesPerLine = 16

func usage() {
	fmt.Println("make-static-content -?")
	fmt.Println("   Prints this help text")
	fmt.Println("make-static-content <packageName> <contentName> <contentType> <contentFormat> <srcFile> <dstFile.go>")
	fmt.Println("   <packageName>   is the name of the ultimate package for <dstFile.go>")
	fmt.Println("   <contentName>   is the basename of the desired content resource")
	fmt.Println("   <contentType>   is the string to record as the static content's Content-Type")
	fmt.Println("   <contentFormat> indicates whether the static content is a string (\"s\") or a []byte (\"b\")")
	fmt.Println("   <srcFile>       is the path to the static content to be embedded")
	fmt.Println("   <dstFile.go>    is the name of the generated .go source file containing:")
	fmt.Println("                     <contentName>ContentType string holding value of <contentType>")
	fmt.Println("                     <contentName>Content     string or []byte holding contents of <srcFile>")
}

var bs = []byte{}

func main() {
	var (
		contentFormat       string
		contentName         string
		contentType         string
		dstFile             *os.File
		dstFileName         string
		err                 error
		packageName         string
		srcFileContentByte  byte
		srcFileContentIndex int
		srcFileContents     []byte
		srcFileName         string
	)

	if (2 == len(os.Args)) && ("-?" == os.Args[1]) {
		usage()
		os.Exit(0)
	}

	if 7 != len(os.Args) {
		usage()
		os.Exit(1)
	}

	packageName = os.Args[1]
	contentName = os.Args[2]
	contentType = os.Args[3]
	contentFormat = os.Args[4]
	srcFileName = os.Args[5]
	dstFileName = os.Args[6]

	srcFileContents, err = ioutil.ReadFile(srcFileName)
	if nil != err {
		panic(err.Error())
	}

	dstFile, err = os.Create(dstFileName)
	if nil != err {
		panic(err.Error())
	}

	_, err = dstFile.Write([]byte(fmt.Sprintf("// Code generated by \"go run make_static_content.go %v %v %v %v %v %v\" - DO NOT EDIT\n\n", packageName, contentName, contentType, contentFormat, srcFileName, dstFileName)))
	if nil != err {
		panic(err.Error())
	}
	_, err = dstFile.Write([]byte(fmt.Sprintf("package %v\n\n", packageName)))
	if nil != err {
		panic(err.Error())
	}

	_, err = dstFile.Write([]byte(fmt.Sprintf("const %vContentType = \"%v\"\n\n", contentName, contentType)))
	if nil != err {
		panic(err.Error())
	}

	switch contentFormat {
	case "s":
		_, err = dstFile.Write([]byte(fmt.Sprintf("const %vContent = `%v`\n", contentName, string(srcFileContents[:]))))
		if nil != err {
			panic(err.Error())
		}
	case "b":
		_, err = dstFile.Write([]byte(fmt.Sprintf("var %vContent = []byte{", contentName)))
		if nil != err {
			panic(err.Error())
		}
		for srcFileContentIndex, srcFileContentByte = range srcFileContents {
			if 0 == (srcFileContentIndex % bytesPerLine) {
				_, err = dstFile.Write([]byte(fmt.Sprintf("\n\t0x%02X,", srcFileContentByte)))
			} else {
				_, err = dstFile.Write([]byte(fmt.Sprintf(" 0x%02X,", srcFileContentByte)))
			}
			if nil != err {
				panic(err.Error())
			}
		}
		_, err = dstFile.Write([]byte("\n}\n"))
		if nil != err {
			panic(err.Error())
		}
	default:
		usage()
		os.Exit(1)
	}

	err = dstFile.Close()
	if nil != err {
		panic(err.Error())
	}

	os.Exit(0)
}

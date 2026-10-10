package lxgo

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// jsUtils 对应真实 lx-music 客户端注入给自定义音源脚本的 `lx.utils`，
// 目前实现脚本实际用到的两块能力：
//   - utils.buffer.from / bufToString：字符串 <-> 字节序列 编解码（utf-8/hex/base64）
//   - utils.crypto.md5 / aesEncrypt / aesDecrypt：部分音源（如 网易云 eapi）需要用到的签名/加密算法
//
// 之所以用"普通类型的 Go 函数"而不是 goja.FunctionCall 风格来实现，是因为 goja 支持
// 通过反射自动把 JS 端传入的 ArrayBuffer / TypedArray / 真·Buffer(由 goja_nodejs/buffer
// 提供) 转换成 Go 的 []byte，也能把 Go 返回的 []byte 自动转换成 JS 可用的 ArrayBuffer，
// 因此不需要手写参数解析，代码更简单也更不容易出错。
func newJSUtils() map[string]interface{} {
	return map[string]interface{}{
		"buffer": map[string]interface{}{
			"from":        bufferFrom,
			"bufToString": bufToString,
		},
		"crypto": map[string]interface{}{
			"md5":        md5Hex,
			"aesEncrypt": aesCrypt(true),
			"aesDecrypt": aesCrypt(false),
		},
	}
}

func normalizeEncoding(encoding string) string {
	e := strings.ToLower(strings.TrimSpace(encoding))
	switch e {
	case "", "utf-8", "utf8":
		return "utf-8"
	case "hex":
		return "hex"
	case "base64":
		return "base64"
	case "binary", "latin1":
		return "binary"
	default:
		return "utf-8"
	}
}

// bufferFrom 对应 utils.buffer.from(str, encoding)，返回的 []byte 会被 goja 自动
// 包装成 JS 端可用的 ArrayBuffer/Buffer。
func bufferFrom(str string, encoding string) ([]byte, error) {
	switch normalizeEncoding(encoding) {
	case "hex":
		return hex.DecodeString(str)
	case "base64":
		return base64.StdEncoding.DecodeString(str)
	case "binary":
		return []byte(str), nil
	default:
		return []byte(str), nil
	}
}

// bufToString 对应 utils.buffer.bufToString(buf, encoding)。buf 可以是我们自己
// bufferFrom/aesEncrypt 返回的 []byte，也可以是脚本自己通过全局 Buffer/Uint8Array
// 构造出来的字节序列——goja 在按 []byte 类型做反射转换时对这几种情况都能处理。
func bufToString(buf []byte, encoding string) string {
	switch normalizeEncoding(encoding) {
	case "hex":
		return hex.EncodeToString(buf)
	case "base64":
		return base64.StdEncoding.EncodeToString(buf)
	default:
		return string(buf)
	}
}

func md5Hex(str string) string {
	sum := md5.Sum([]byte(str))
	return hex.EncodeToString(sum[:])
}

// aesCrypt 返回 utils.crypto.aesEncrypt / aesDecrypt 的实现。
// mode 形如 "aes-128-ecb"、"aes-256-cbc"，只区分 ecb/cbc 两种最常见的分组模式，
// 缺省按 PKCS7 补齐/去除填充（对应 Node crypto 默认的自动 padding 行为）。
func aesCrypt(encrypt bool) func(data string, mode string, key string, iv string) ([]byte, error) {
	return func(data string, mode string, key string, iv string) ([]byte, error) {
		block, err := aes.NewCipher([]byte(key))
		if err != nil {
			return nil, fmt.Errorf("aes key invalid (len=%d): %w", len(key), err)
		}
		bs := block.BlockSize()
		ivBytes := padOrTrim([]byte(iv), bs)

		m := strings.ToLower(mode)
		switch {
		case strings.Contains(m, "ecb"):
			if encrypt {
				plain := pkcs7Pad([]byte(data), bs)
				out := make([]byte, len(plain))
				for i := 0; i < len(plain); i += bs {
					block.Encrypt(out[i:i+bs], plain[i:i+bs])
				}
				return out, nil
			}
			in := []byte(data)
			if len(in)%bs != 0 {
				return nil, fmt.Errorf("aes-ecb ciphertext is not a multiple of block size")
			}
			out := make([]byte, len(in))
			for i := 0; i < len(in); i += bs {
				block.Decrypt(out[i:i+bs], in[i:i+bs])
			}
			return pkcs7Unpad(out)
		case strings.Contains(m, "cbc"):
			if encrypt {
				plain := pkcs7Pad([]byte(data), bs)
				out := make([]byte, len(plain))
				cipher.NewCBCEncrypter(block, ivBytes).CryptBlocks(out, plain)
				return out, nil
			}
			in := []byte(data)
			if len(in)%bs != 0 {
				return nil, fmt.Errorf("aes-cbc ciphertext is not a multiple of block size")
			}
			out := make([]byte, len(in))
			cipher.NewCBCDecrypter(block, ivBytes).CryptBlocks(out, in)
			return pkcs7Unpad(out)
		default:
			return nil, fmt.Errorf("unsupported aes mode: %s", mode)
		}
	}
}

func padOrTrim(b []byte, size int) []byte {
	if len(b) >= size {
		return b[:size]
	}
	out := make([]byte, size)
	copy(out, b)
	return out
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padLen := blockSize - len(data)%blockSize
	return append(bytes.Clone(data), bytes.Repeat([]byte{byte(padLen)}, padLen)...)
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	padLen := int(data[len(data)-1])
	if padLen <= 0 || padLen > len(data) {
		return nil, fmt.Errorf("invalid pkcs7 padding")
	}
	return data[:len(data)-padLen], nil
}

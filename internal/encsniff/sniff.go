package encsniff

import (
	"bytes"
	"fmt"
	"math"
	stdunicode "unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

const LowConfidence = 0.70

type Result struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
	Text       []byte  `json:"-"`
	Low        bool    `json:"low_confidence,omitempty"`
}

type candidate struct {
	name       string
	decoder    *encoding.Decoder
	confidence float64
	text       []byte
	ok         bool
}

func Detect(data []byte) (Result, error) {
	if len(data) == 0 {
		return Result{Name: "UTF-8", Confidence: 1, Text: []byte{}}, nil
	}
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		return Result{Name: "UTF-8-BOM", Confidence: 1, Text: data[3:]}, nil
	}
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		return detectUTF16(data)
	}
	if utf8.Valid(data) {
		return Result{Name: "UTF-8", Confidence: 1, Text: data}, nil
	}
	candidates := []candidate{
		{name: "GB18030", decoder: simplifiedchinese.GB18030.NewDecoder()},
		{name: "Big5", decoder: traditionalchinese.Big5.NewDecoder()},
		{name: "Shift-JIS", decoder: japanese.ShiftJIS.NewDecoder()},
	}
	best := -1
	for i := range candidates {
		text, _, err := transform.Bytes(candidates[i].decoder, data)
		if err != nil || !utf8.Valid(text) {
			candidates[i].confidence = 0.05
			candidates[i].text = []byte(string(data))
			continue
		}
		candidates[i].ok = true
		candidates[i].text = text
		candidates[i].confidence = score(text)
		if best < 0 || candidates[i].confidence > candidates[best].confidence {
			best = i
		}
	}
	if best < 0 {
		return Result{Name: "unknown", Confidence: 0.05, Low: true, Text: []byte(string(data))}, nil
	}
	selected := candidates[best]
	if !selected.ok {
		return Result{Name: selected.name, Confidence: selected.confidence, Low: true, Text: selected.text}, nil
	}
	return Result{Name: selected.name, Confidence: round3(selected.confidence),
		Low: selected.confidence < LowConfidence, Text: selected.text}, nil
}

func detectUTF16(data []byte) (Result, error) {
	order := xunicode.LittleEndian
	name := "UTF-16LE"
	body := data[2:]
	if bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		order = xunicode.BigEndian
		name = "UTF-16BE"
	}
	decoder := xunicode.UTF16(order, xunicode.IgnoreBOM).NewDecoder()
	text, _, err := transform.Bytes(decoder, body)
	if err != nil {
		return Result{Name: name, Confidence: 0, Text: body}, fmt.Errorf("decode %s: %w", name, err)
	}
	return Result{Name: name, Confidence: 1, Text: text}, nil
}

func score(text []byte) float64 {
	runes := []rune(string(text))
	if len(runes) == 0 {
		return 0
	}
	var total, cjk, common, kana, defects float64
	for _, r := range runes {
		switch {
		case r == utf8.RuneError:
			weight(r, &total, 0)
			defects += 10
		case stdunicode.Is(stdunicode.Co, r), stdunicode.IsControl(r) && !stdunicode.IsSpace(r):
			weight(r, &total, 0)
			defects += 6
		case stdunicode.Is(stdunicode.Han, r):
			cjk++
			if commonHan[r] {
				common++
				weight(r, &total, 3.2)
			} else {
				weight(r, &total, 1.1)
			}
		case r >= 0xff61 && r <= 0xff9f:
			weight(r, &total, 0.8)
		case stdunicode.Is(stdunicode.Hiragana, r), stdunicode.Is(stdunicode.Katakana, r):
			kana++
			weight(r, &total, 3.5)
		case stdunicode.Is(stdunicode.Hangul, r):
			cjk++
			weight(r, &total, 2.8)
		case r < stdunicode.MaxASCII && stdunicode.IsPrint(r), stdunicode.IsSpace(r):
			weight(r, &total, 2.4)
		case stdunicode.IsPunct(r), stdunicode.IsSymbol(r):
			weight(r, &total, 1.5)
		default:
			weight(r, &total, 0.8)
		}
	}
	quality := total / float64(len(runes))
	quality -= 3.5 * defects / float64(len(runes))
	if cjk > 0 {
		ratio := common / cjk
		quality *= 0.72 + 0.28*ratio
		if kana > 0 && cjk > 0 {
			quality *= 1.08
		}
	}
	return clamp(quality / 3.0)
}

func weight(_ rune, total *float64, value float64) {
	*total += value
}

func clamp(value float64) float64 {
	return math.Max(0, math.Min(1, value))
}

func round3(value float64) float64 {
	return math.Round(value*1000) / 1000
}

var commonHan = map[rune]bool{}

func init() {
	common := "的一是不了在人有我他这个们中来上大为和国地到以说时要就出会可也你对生能而子那得于着下自之年过发后作里用道行所然家种事成方多经么去法学如都同现当没动面起看定天分还进好小部其些主样理心她本前开但因只从想实日军者意无力它与长把机十民第公此已工使情明性知全三又关点正业外将两高间由问很最重并物手应战向头文体政美相见被利什二等产或新己制身果加西斯月话合回特代内信表化老给世位次度门任常先海通教儿原东声提立及比员解水名真论处走义各入几口认条平系气题活尔更别打女变四神总何电数安少报才结反受目太量再感建务做接必场件计管期市直德资命山金指克许统区保至队形社便空决治展马科司五基眼书非则听白却界达光放强即像难且权思王象完设式色路记南品住告类求据程北边死张该交规万取拉格望觉术领共确传师观清今切院让识候带导争运笑飞风步改收根干造言联持组每济车亲极林服快办议往元英士证近失转夫令准布始怎呢存未远叫台单影具罗字爱击流备兵连调深商算质团集百需价花党华城石级整府离况亚请技际约示复病息究线似官火断精满支视消越器容照须九增研写称企八功吗包片史委乎查轻易早曾除农找装广显吧阿李标谈吃图念六引历首医局突专费号尽另周较注语仅考落青随选列武红响虽推势参希古众构房半节土投某案黑维革划敌致陈律足态护七兴派孩验责营星够章音跟志底站严巴例防族供效续施留讲型料终答紧黄绝奇察母京段依批群项故按河米封液旅"
	traditional := "這個們來為國地說時就會對與長機氣還進開關點業兩間問體見實軍無從頭戰東聲員處義認條題爾與變總電數報結應場務計劃資統隊決馬書聽卻達難權歲設記類據邊張該規萬覺術師觀讓識帶導爭運飛風聯組濟親極辦議證轉準遠單影羅愛擊備連調質團價華級離請際約復線醫專費盡週註語隨選響雖參眾構節裝廣顯標談歷醫局衛麼週歲龍鳳寶"
	for _, r := range common + traditional {
		commonHan[r] = true
	}
}

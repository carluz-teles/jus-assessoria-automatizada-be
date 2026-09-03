package draft

import (
	"strings"

	"golang.org/x/net/html"
)

// stripHTMLTagsForSearch derives plain-text from a rich-HTML draft body, used
// as the `content` legacy column (backup pra full-text search e export TXT).
// Não é 1:1 com o layout do editor — apenas texto concatenado com quebras de
// linha entre blocos. Tags desconhecidas viram texto inline; <br> vira "\n".
func stripHTMLTagsForSearch(htmlStr string) string {
	doc, err := html.Parse(strings.NewReader("<div>" + htmlStr + "</div>"))
	if err != nil || doc == nil {
		return htmlStr // fallback: devolve cru; melhor isso que perder o conteúdo
	}
	var sb strings.Builder
	var visit func(n *html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			return
		}
		blockTag := isBlockTag(n.Data)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
		if blockTag {
			sb.WriteString("\n")
		}
	}
	visit(doc)
	// Normaliza espaços em branco: colapsa múltiplas quebras + trim.
	out := strings.TrimSpace(sb.String())
	// Colapsa 3+ quebras consecutivas em 2 (mantém parágrafos separados).
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}

// parseHTMLToStructured deriva um StructuredContent a partir do HTML gerado
// pela IA (v7). Convenção: cada <h1>/<h2>/<h3> abre uma seção; parágrafos
// anteriores ao 1º heading viram Preamble; parágrafos/listas/tabelas dentro
// de uma seção viram Section.Paragraphs (texto flat, sem formatação inline).
//
// Usado como fallback pro iterate legacy (que opera por seção). Quando o
// iterate for reescrito pra operar em HTML direto, essa função morre.
func parseHTMLToStructured(htmlStr string) *StructuredContent {
	if strings.TrimSpace(htmlStr) == "" {
		return nil
	}
	doc, err := html.Parse(strings.NewReader("<div>" + htmlStr + "</div>"))
	if err != nil || doc == nil {
		return nil
	}
	preamble := StructuredPreamble{}
	var sections []StructuredSection
	var current *StructuredSection
	seen := map[string]bool{} // garante ids únicos na peça (stableSectionID)

	var visit func(n *html.Node)
	visit = func(n *html.Node) {
		if n.Type != html.ElementNode {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				visit(c)
			}
			return
		}
		tag := strings.ToLower(n.Data)
		switch tag {
		case "h1", "h2", "h3":
			text := strings.TrimSpace(nodeText(n))
			roman, title := parseSectionHeading(text)
			id := stableSectionID(roman, len(sections)+1, seen)
			shortTitle := shortTitleFromTitle(title, roman)
			sections = append(sections, StructuredSection{
				ID:         id,
				Roman:      roman,
				Title:      title,
				ShortTitle: shortTitle,
			})
			current = &sections[len(sections)-1]
			return
		case "p", "blockquote", "ul", "ol", "table":
			text := strings.TrimSpace(nodeText(n))
			if text == "" {
				return
			}
			if tag == "p" {
				if m := headingRE.FindStringSubmatch(text); m != nil {
					roman, title := m[1], strings.TrimSpace(m[2])
					id := stableSectionID(roman, len(sections)+1, seen)
					shortTitle := shortTitleFromTitle(title, roman)
					sections = append(sections, StructuredSection{
						ID:         id,
						Roman:      roman,
						Title:      title,
						ShortTitle: shortTitle,
					})
					current = &sections[len(sections)-1]
					return
				}
			}
			if current != nil {
				current.Paragraphs = append(current.Paragraphs, text)
			} else {
				preamble.Paragraphs = append(preamble.Paragraphs, text)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(doc)

	if len(preamble.Paragraphs) == 0 && len(sections) == 0 {
		return nil
	}
	return &StructuredContent{Preamble: preamble, Sections: sections}
}

// nodeText coleta o texto concatenado de um nó html (inclui filhos), com
// quebra dupla entre blocos aninhados (<li>, <tr>) pra preservar leitura.
func nodeText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			sb.WriteString(x.Data)
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if x.Type == html.ElementNode {
			switch strings.ToLower(x.Data) {
			case "li", "tr", "br":
				sb.WriteString("\n")
			}
		}
	}
	walk(n)
	return sb.String()
}

// parseSectionHeading extrai roman + título de textos como
// "I — DOS FATOS" ou "II - DO DIREITO" ou apenas "PRELIMINARES".
func parseSectionHeading(text string) (roman, title string) {
	// tenta padrão "<roman>[ — - :]<title>"
	for _, sep := range []string{"—", "–", "-", ":"} {
		if i := strings.Index(text, sep); i > 0 {
			head := strings.TrimSpace(text[:i])
			tail := strings.TrimSpace(text[i+len(sep):])
			if isRoman(head) {
				return head, tail
			}
		}
	}
	return "", text
}

func isRoman(s string) bool {
	if s == "" || len(s) > 6 {
		return false
	}
	for _, r := range s {
		switch r {
		case 'I', 'V', 'X', 'L', 'C', 'D', 'M':
			continue
		default:
			return false
		}
	}
	return true
}

func isBlockTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "p", "h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "li", "blockquote", "table", "tr", "td", "th", "br", "hr", "div":
		return true
	}
	return false
}

// stableSectionID gera o id ESTÁVEL de uma seção: o algarismo romano em minúsculo
// ("i", "ii", "iii", "iv") — que NÃO muda se o título da seção for editado entre
// gerações — com fallback pela POSIÇÃO ("s1", "s2"…) quando não há romano OU
// quando aquele romano já foi usado nesta peça. `seen` garante unicidade (a
// posição `ord` é sempre única no passe de parse), o que os consumidores exigem
// (o byID map do /iterate colidiria silenciosamente com ids repetidos).
func stableSectionID(roman string, ord int, seen map[string]bool) string {
	id := strings.ToLower(strings.TrimSpace(roman))
	if id == "" || seen[id] {
		id = "s" + itoa(ord)
	}
	seen[id] = true
	return id
}

func shortTitleFromTitle(title, roman string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		return roman
	}
	// pega última palavra (ex.: "DOS FATOS" → "FATOS")
	parts := strings.Fields(t)
	if len(parts) == 0 {
		return roman
	}
	return parts[len(parts)-1]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

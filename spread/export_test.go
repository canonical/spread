package spread

func Strmap(context string, strings []string) strmap {
	return strmap{context: stringer(context), strings: strings}
}

var (
	Evalstr = evalstr
)

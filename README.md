# Elephant spell

Elephant spell is a spellcheck service based on hunspell.

## Custom dictionary support

Register words or phrases in the custom dictionary using the Dictionary API:

``` json
POST twirp/elephant.spell.Dictionaries/SetEntry

{
  "entry": {
    "language": "sv-se",
    "text": "Belarus",
    "status": "approved",
    "description": "Vitryssland var det gamla namnet på Belarus",
    "common_mistakes": [
        "Vitryssland"
    ]
  }
}
```

The custom dictionary can be used both to add previously unknown words, and to encourage the replacement of words that doesn't follow your language guidelines.

Given the custom entries:

``` json
{
  "entries": [
    {
      "language": "sv-se",
      "text": "fängelse",
      "status": "approved",
      "description": "Skriv fängelse och inte kriminalvårdsanstalt.",
      "common_mistakes": [
        "kriminalvårdsanstalt"
      ],
      "level": "LEVEL_ERROR",
      "forms": {
        "kriminalvårdsanstalten": "fängelset",
        "kriminalvårdsanstalter": "fängelser"
      }
    },
    {
      "language": "sv-se",
      "text": "Muammar Gaddafi",
      "status": "approved",
      "common_mistakes": [
        "{Mohammar|Mohammer|Muammar|Muhammar|Muhammer} {Gadaffi|Ghadaffi|Ghadafi|Kadhaffi|Kadhafi|Khadaffi}"
      ],
      "level": "LEVEL_ERROR"
    },
    {
      "language": "sv-se",
      "text": "relik",
      "status": "approved",
      "description": "Relik har religiös betydelse. En kroppsdel eller ett föremål som vördas. Relikt är mer allmänt en kvarleva.",
      "common_mistakes": [
        "relikt"
      ],
      "level": "LEVEL_SUGGESTION"
    },
    {
      "language": "sv-se",
      "text": "Belarus",
      "status": "approved",
      "description": "Vitryssland var det gamla namnet på Belarus",
      "common_mistakes": [
        "Vitryssland"
      ],
      "level": "LEVEL_ERROR"
    }
  ]
}
```

You can call the spellcheck method:

``` json
POST twirp/elephant.spell.Check/Text

{
  "language": "sv-se",
  "text": [
    "Nu går vi till kriminalvårdsanstalten.",
    "En riktig relikt!",
    "Hette han Mohammar Gadaffi?",
    "Ska man ressa till Vitryssland?"
  ]
}
```

...and get corrections using both the built in Swedish dictionary and your custom dictionary:

``` json
{
  "misspelled": [
    {
      "entries": [
        {
          "text": "kriminalvårdsanstalten",
          "level": "LEVEL_ERROR"
        }
      ]
    },
    {
      "entries": [
        {
          "text": "relikt",
          "level": "LEVEL_SUGGESTION"
        }
      ]
    },
    {
      "entries": [
        {
          "text": "Mohammar Gadaffi",
          "level": "LEVEL_ERROR"
        }
      ]
    },
    {
      "entries": [
        {
          "text": "Vitryssland",
          "level": "LEVEL_ERROR"
        },
        {
          "text": "ressa",
          "level": "LEVEL_ERROR"
        }
      ]
    }
  ]
}
```

To get the suggestions for each of these entries, call `Suggestions`:

``` json
POST twirp/elephant.spell.Check/Suggestions

{
  "text": "ressa",
  "language": "sv-se"
}

{
  "suggestions": [
    {
      "text": "resas"
    },
    {
      "text": "resa"
    },
    {
      "text": "dressa"
    },
    {
      "text": "pressa"
    }
  ]
}
```

``` json
{
  "text": "kriminalvårdsanstalten",
  "language": "sv-se"
}

{
  "suggestions": [
    {
      "text": "fängelset",
      "description": "Skriv fängelse och inte kriminalvårdsanstalt."
    }
  ]
}
```

## Supported languages

We currently bundle the following dictionaries:

* British English
* Danish
* Finnish
* Norwegian Bokmål
* Norwegian Nynorsk
* Swedish
* US English

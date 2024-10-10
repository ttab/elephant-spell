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

Then you can call the spellcheck method:

``` json
POST twirp/elephant.spell.Check/Text

{
  "language": "sv-se",
  "text": "Detta är text som är rätstavad, nej, jag menar rätsstavad! Vitryssland är ett land i Europa."
}
```

...and get corrections using both the built in Swedish dictionary and your custom dictionary:

``` json
{
  "misspelled": [
    {
      "text": "Vitryssland",
      "suggestions": [
        {
          "text": "Belarus",
          "description": "Vitryssland var det gamla namnet på Belarus"
        }
      ]
    },
    {
      "text": "rätstavad",
      "suggestions": [
        {
          "text": "rättstavad"
        }
      ]
    },
    {
      "text": "rätsstavad",
      "suggestions": [
        {
          "text": "rättstavad"
        },
        {
          "text": "lättstavad"
        },
        {
          "text": "rättsstat"
        },
        {
          "text": "vadarstav"
        },
        {
          "text": "stadsrätt"
        }
      ]
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

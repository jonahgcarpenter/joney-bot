# comfyui.text_to_image

## Description

Generate one image from a concise, concrete visual description. Put the subject and required attributes first, including visible features that distinguish it from similar objects. Describe composition, lighting, and style when relevant to the user's request; do not add unrequested embellishments. Avoid conversational instructions, contradictory styles, and piles of generic quality keywords. Use a short, targeted negative_prompt only for unwanted visual elements, not a universal negative list. The returned image is attached to the response.

For example, describe a fire engine with external hose compartments, equipment lockers, emergency lights, and a side ladder rather than relying only on "truck". If passenger-bus features are unwanted, a targeted negative prompt can name "passenger bus, rows of passenger windows". Precise descriptions help conditioning but do not guarantee object geometry or readable text.

Each successful call creates a new logical image with server-assigned image_id and version 1. Refine it using image_to_image rather than creating repeated drafts with this tool. Only the latest successful version per logical image produced this request is attached at final delivery. At most four logical images can be delivered per request; intermediate versions remain temporary editing sources. Result source_image_id is the exact immutable asset selector, distinct from image_id.

## Parameters

| Name | Type | Required | Description |
| --- | --- | --- | --- |
| prompt | string | yes | Concise visual description: subject and required distinguishing attributes first, then relevant composition, lighting, and style |
| negative_prompt | string | no | Short, targeted list of unwanted visual elements; avoid generic negative lists |

## Schema

```json
{
  "type": "object",
  "properties": {
    "prompt": {"type": "string", "description": "Concise visual description with subject and required distinguishing attributes first, then relevant composition, lighting, and style. Avoid conflicting styles, generic quality keywords, and unrequested embellishments", "minLength": 1, "maxLength": 2000},
    "negative_prompt": {"type": "string", "description": "Short, targeted list of unwanted visual elements; avoid generic negative lists", "maxLength": 2000}
  },
  "required": ["prompt"],
  "additionalProperties": false
}
```

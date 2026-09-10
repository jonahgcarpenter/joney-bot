# comfyui.image_to_image

## Description

Transform an available image according to a detailed visual prompt. Select source_image_id from the session image catalog. If omitted, use the latest output generated in this request, otherwise the first current attached/replied image, otherwise the latest retained delivered output. If no source is available, ask for an image or generate one first. The returned image is attached to the response. Use negative_prompt only to describe visual elements that should be excluded.

## Parameters

| Name | Type | Required | Description |
| --- | --- | --- | --- |
| prompt | string | yes | Detailed positive description of the requested transformation |
| negative_prompt | string | no | Visual elements and qualities to exclude |
| source_image_id | string | no | Available image ID from the session image catalog |

## Schema

```json
{
  "type": "object",
  "properties": {
    "source_image_id": {"type": "string", "description": "Available image ID from the session image catalog"},
    "prompt": {"type": "string", "description": "Detailed positive description of the requested transformation", "minLength": 1, "maxLength": 2000},
    "negative_prompt": {"type": "string", "description": "Visual elements and qualities to exclude", "maxLength": 2000}
  },
  "required": ["prompt"],
  "additionalProperties": false
}
```

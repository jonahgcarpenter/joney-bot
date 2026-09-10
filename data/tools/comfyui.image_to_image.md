# comfyui.image_to_image

## Description

Transform an available image according to a detailed visual prompt. Describe the desired FINAL image, emphasizing the changed attribute and what should remain, rather than a bare edit command (for example, "a cobalt-blue car, blue body panels, same side view and street background" rather than "make it blue"). Select source_image_id from the session image catalog. If omitted, use the latest output generated in this request, otherwise the first current attached/replied image, otherwise the latest retained delivered output. If no source is available, ask for an image or generate one first. Optional strength controls denoise from 0.1 to 0.9; omission preserves the operator template. Consider around 0.55-0.65 for visible changes, without guarantees; higher values can alter composition and identity. The returned image is attached to the response. Use negative_prompt only to describe visual elements that should be excluded.

By default, an edit creates the next server-assigned version of the selected logical image, replacing its earlier output in this response. Editing an older source still advances the version; parent_source_image_id identifies that exact source. Set create_variant=true to keep both alternatives as separate logical images. Only the latest successful version of each logical image produced this request is attached at final delivery (maximum four logical images). Failed edits keep the last success. Results include image_id, version, source_image_id, and parent_source_image_id; use only catalog source_image_id values for editing.

## Parameters

| Name | Type | Required | Description |
| --- | --- | --- | --- |
| prompt | string | yes | Desired final image, emphasizing the changed attribute and details to retain, not a bare edit command |
| negative_prompt | string | no | Visual elements and qualities to exclude |
| source_image_id | string | no | Available image ID from the session image catalog |
| create_variant | boolean | no | Create a separate logical image rather than replacing the selected image's output; defaults false |
| strength | number | no | Denoise from 0.1 to 0.9; omitted uses template. Consider 0.55-0.65 for visible changes; higher values can alter composition and identity |

## Schema

```json
{
  "type": "object",
  "properties": {
    "create_variant": {"type": "boolean", "description": "Create a separate logical image to deliver both alternatives; default false advances the selected logical image's version"},
    "source_image_id": {"type": "string", "description": "Available image ID from the session image catalog"},
    "strength": {"type": "number", "description": "Denoise from 0.1 to 0.9; omission preserves operator template. Consider 0.55-0.65 for visible changes, without guarantees; higher values can alter composition and identity", "minimum": 0.1, "maximum": 0.9},
    "prompt": {"type": "string", "description": "Describe the desired FINAL image, emphasizing the changed attribute and details to retain, not a bare edit command", "minLength": 1, "maxLength": 2000},
    "negative_prompt": {"type": "string", "description": "Visual elements and qualities to exclude", "maxLength": 2000}
  },
  "required": ["prompt"],
  "additionalProperties": false
}
```

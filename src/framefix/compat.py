"""Small compatibility helpers for PyAV 12.3 through 18.x."""

from __future__ import annotations

import av


def add_stream_like(output_container: av.container.OutputContainer, input_stream: av.Stream):
    """Copy a source stream template using the API available in the PyAV build."""
    factory = getattr(output_container, "add_stream_from_template", None)
    if factory is not None:
        return factory(input_stream)
    return output_container.add_stream(template=input_stream)

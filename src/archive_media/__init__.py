"""Archive media recovery package.

The data flow is: manifest parsing -> ordered segment plan -> streaming
demux -> unified timeline -> audio processing -> segmented archive, with the
index and verification modules consuming the same plan and error model.
"""

__version__ = "0.1.0"

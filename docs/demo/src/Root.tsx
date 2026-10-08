import {Composition} from 'remotion';
import {Demo, totalFrames} from './Demo';

export const Root = () => (
  <Composition
    id="Demo"
    component={Demo}
    durationInFrames={totalFrames}
    fps={20}
    width={960}
    height={560}
  />
);
